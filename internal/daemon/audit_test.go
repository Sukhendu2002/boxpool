//go:build linux || darwin

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"boxpool.run/boxpool/internal/auth"
	"boxpool.run/boxpool/internal/store"
)

func auditEvents(t *testing.T, st *store.Store) []store.AuditEvent {
	t.Helper()
	list, err := st.ListAudit(context.Background(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestAuditEndpoints(t *testing.T) {
	fakeServe(t)
	_, d, st := newAPIStore(t)
	s := newServer(Info{StateDir: filepath.Dir(d.sock)}, st, d)
	t.Cleanup(s.closeExposes)
	head := httptest.NewServer(s.handler())
	t.Cleanup(head.Close)
	remote := httptest.NewServer(s.remote(s.handler()))
	t.Cleanup(remote.Close)
	start := time.Now()
	count := 0
	want := func(action, target, actor string) {
		t.Helper()
		count++
		list := auditEvents(t, st)
		if len(list) != count {
			t.Fatalf("%s: have %d events, want %d: %+v", action, len(list), count, list)
		}
		e := list[0]
		if e.Action != action || e.Target != target || e.TokenID != actor || e.Time.Before(start) || e.Time.After(time.Now()) {
			t.Fatalf("event = %+v, want %s %s %s", e, action, target, actor)
		}
	}
	call := func(token, method, path, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, remote.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	code, out := send(t, head, "POST", "/v1/access-tokens", `{"scope":"cli","sandbox":"web"}`)
	var tok AccessToken
	if json.Unmarshal([]byte(out), &tok) != nil || code != 201 {
		t.Fatalf("token = %d %s", code, out)
	}
	want("token.create", tok.ID, "local")
	if code, body := call(tok.Token, "POST", "/v1/login", ""); code != 204 {
		t.Fatalf("login = %d %s", code, body)
	}
	want("login", tok.ID, tok.ID)
	if code, body := call(tok.Token, "POST", "/v1/sandboxes", `{"name":"web"}`); code != 201 {
		t.Fatalf("create = %d %s", code, body)
	}
	want("sandbox.create", "web", tok.ID)
	// Existing sandboxes, health checks, failed/denied operations and audit
	// reads must not create misleading successful-action entries.
	for _, tc := range []struct {
		token, method, path, body string
		code                      int
	}{
		{tok.Token, "POST", "/v1/sandboxes", `{"name":"web"}`, 200},
		{tok.Token, "GET", "/v1/health", "", 200},
		{tok.Token, "POST", "/v1/sandboxes", `{"name":"other"}`, 403},
		{tok.Token, "GET", "/v1/audit", "", 403},
		{tok.Token, "DELETE", "/v1/access-tokens/" + tok.ID, "", 403},
		{"invalid", "POST", "/v1/login", "", 401},
	} {
		if code, body := call(tc.token, tc.method, tc.path, tc.body); code != tc.code {
			t.Fatalf("%s %s = %d %s", tc.method, tc.path, code, body)
		}
	}
	if code, _ := send(t, head, "GET", "/v1/sandboxes/missing/exec", ""); code != 404 {
		t.Fatalf("missing exec = %d", code)
	}
	if code, _ := send(t, head, "POST", "/v1/sandboxes", `{"name":"Bad"}`); code != 400 {
		t.Fatalf("bad create = %d", code)
	}
	if code, _ := send(t, head, "POST", "/v1/login", ""); code != 401 {
		t.Fatalf("local login = %d", code)
	}
	if len(auditEvents(t, st)) != count {
		t.Fatal("failed requests or health checks were audited")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, remote.URL+"/v1/sandboxes/web/exec", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + tok.Token}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	want("sandbox.exec", "web", tok.ID) // visible while the shell is still open
	c.Close(websocket.StatusNormalClosure, "")

	port := app(t)
	e, base := expose(t, head, fmt.Sprintf(`{"sandbox":"web","port":%d}`, port))
	count++ // an expose also creates its own access token
	want("sandbox.expose", "web:"+strconv.Itoa(port), "local")
	if list := auditEvents(t, st); list[1].Action != "token.create" || list[1].Target != e.TokenID {
		t.Fatalf("expose token = %+v", list[1])
	}
	if resp, _ := get(t, base+"/private?secret=do-not-record"); resp.StatusCode != 401 {
		t.Fatalf("unauthenticated expose = %d", resp.StatusCode)
	}
	if len(auditEvents(t, st)) != count {
		t.Fatal("unauthenticated expose was audited")
	}
	link, _ := auth.ParseAccessToken(strings.SplitN(e.URL, "?"+tokenParam+"=", 2)[1])
	resp, _ := get(t, base+"/private?"+tokenParam+"="+link.String()+"&secret=do-not-record")
	if resp.StatusCode != 303 {
		t.Fatalf("expose login = %d", resp.StatusCode)
	}
	want("sandbox.access", "web:"+strconv.Itoa(port), e.TokenID)
	if resp, _ := get(t, base+"/private", resp.Cookies()...); resp.StatusCode != 200 {
		t.Fatalf("expose cookie = %d", resp.StatusCode)
	}
	want("sandbox.access", "web:"+strconv.Itoa(port), e.TokenID)
	if code, body := send(t, head, "DELETE", "/v1/exposes/web/"+strconv.Itoa(port), ""); code != 200 {
		t.Fatalf("unexpose = %d %s", code, body)
	}
	count++
	want("sandbox.unexpose", "web:"+strconv.Itoa(port), "local")
	if list := auditEvents(t, st); list[1].Action != "token.revoke" || list[1].Target != e.TokenID {
		t.Fatalf("unexpose token = %+v", list[1])
	}

	if code, body := call(tok.Token, "DELETE", "/v1/sandboxes/web?force=true", ""); code != 200 {
		t.Fatalf("rm = %d %s", code, body)
	}
	want("sandbox.rm", "web", tok.ID)
	if code, body := send(t, head, "DELETE", "/v1/access-tokens/"+tok.ID, ""); code != 204 {
		t.Fatalf("revoke = %d %s", code, body)
	}
	want("token.revoke", tok.ID, "local")

	code, out = send(t, head, "POST", "/v1/tokens", `{}`)
	var join Token
	json.Unmarshal([]byte(out), &join)
	jt, err := auth.ParseJoinToken(join.Token)
	if code != 201 || err != nil {
		t.Fatalf("join token = %d %s", code, out)
	}
	want("token.create", jt.Hash()[:store.AccessIDLen], "local")
	if code, body := send(t, head, "DELETE", "/v1/tokens/"+jt.Hash(), ""); code != 204 {
		t.Fatalf("join revoke = %d %s", code, body)
	}
	want("token.revoke", jt.Hash()[:store.AccessIDLen], "local")
	code, sb, body := post(t, head, `{}`)
	if code != 201 {
		t.Fatalf("generated create = %d %s", code, body)
	}
	want("sandbox.create", sb.Name, "local")

	code, out = send(t, head, "GET", "/v1/audit?limit=1", "")
	var list []store.AuditEvent
	if json.Unmarshal([]byte(out), &list) != nil || code != 200 || len(list) != 1 || list[0].Target != sb.Name {
		t.Fatalf("audit = %d %s", code, out)
	}
	code, out = send(t, head, "GET", fmt.Sprintf("/v1/audit?before=%d&limit=1", list[0].ID), "")
	if json.Unmarshal([]byte(out), &list) != nil || code != 200 || len(list) != 1 || list[0].Action != "token.revoke" {
		t.Fatalf("older audit = %d %s", code, out)
	}
	for _, q := range []string{"limit=0", "limit=1001", "limit=bad", "before=-1", "before=bad"} {
		if code, body := send(t, head, "GET", "/v1/audit?"+q, ""); code != 400 {
			t.Errorf("%s = %d %s", q, code, body)
		}
	}
	all, _ := json.Marshal(auditEvents(t, st))
	if strings.Contains(string(all), tok.Token) || strings.Contains(string(all), join.Token) || strings.Contains(string(all), link.String()) || strings.Contains(string(all), "do-not-record") {
		t.Fatal("audit contains a token secret")
	}
}

func TestAuditFailure(t *testing.T) {
	_, s := machine(t, "linux")
	h := s.audit("sandbox.rm", func(w http.ResponseWriter, r *http.Request) {
		// Simulate an action completing, then its audit write failing.
		s.st.Close()
		w.WriteHeader(http.StatusNoContent)
	})
	r := httptest.NewRequest("DELETE", "/v1/sandboxes/web", nil)
	r.SetPathValue("name", "web")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != 500 || !strings.Contains(w.Body.String(), "operation completed but audit failed") {
		t.Fatalf("audit failure = %d %s", w.Code, w.Body.String())
	}
}

func TestAuditOnNode(t *testing.T) {
	fakeServe(t)
	fastPool(t)
	head, hs, _ := poolMachine(t, "linux")
	t.Cleanup(hs.closeExposes)
	pool := poolServer(t, hs, nil)
	node, ns, _ := poolMachine(t, "darwin")
	if code, body := send(t, node, "POST", "/v1/pool/join", `{"token":"`+issueToken(t, head, pool)+`"}`); code != 200 {
		t.Fatalf("join = %d %s", code, body)
	}
	stop := serveNode(ns)
	defer stop()
	waitFor(t, "the node's first heartbeat", func() bool { return nodeState(t, head) == "ready" })
	code, body := send(t, head, "POST", "/v1/access-tokens", `{"scope":"cli"}`)
	var tok AccessToken
	if code != 201 || json.Unmarshal([]byte(body), &tok) != nil {
		t.Fatalf("create token = %d %s", code, body)
	}
	remote := httptest.NewServer(hs.remote(hs.handler()))
	t.Cleanup(remote.Close)
	call := func(method, path, body string) {
		t.Helper()
		req, _ := http.NewRequest(method, remote.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("%s %s = %d %s", method, path, resp.StatusCode, b)
		}
	}
	call("POST", "/v1/sandboxes", `{"name":"mac","os":"macos"}`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, remote.URL+"/v1/sandboxes/mac/exec", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + tok.Token}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	list := auditEvents(t, hs.st)
	if list[0].Action != "sandbox.exec" || list[0].TokenID != tok.ID || list[0].Target != "mac" {
		t.Fatalf("forwarded exec = %+v", list[0])
	}
	c.Close(websocket.StatusNormalClosure, "")
	e, base := expose(t, head, fmt.Sprintf(`{"sandbox":"mac","port":%d,"public":true}`, app(t)))
	if resp, _ := get(t, base+"/secret-path?password=do-not-record"); resp.StatusCode != 200 {
		t.Fatalf("public access = %d", resp.StatusCode)
	}
	list = auditEvents(t, hs.st)
	if list[0].Action != "sandbox.access" || list[0].TokenID != "public" || list[0].Target != "mac:"+strconv.Itoa(e.Port) {
		t.Fatalf("public access = %+v", list[0])
	}
	call("DELETE", "/v1/sandboxes/mac?force=true", "")
	var actions []string
	for _, e := range auditEvents(t, hs.st) {
		if e.TokenID == tok.ID {
			actions = append(actions, e.Action+":"+e.Target)
		}
	}
	if strings.Join(actions, ",") != "sandbox.rm:mac,sandbox.exec:mac,sandbox.create:mac" {
		t.Fatalf("forwarded actions = %v", actions)
	}
	if list := auditEvents(t, ns.st); len(list) != 0 {
		t.Fatalf("node duplicated head events: %+v", list)
	}
	if code, _ := send(t, node, "GET", "/v1/audit", ""); code != 409 {
		t.Fatalf("node audit = %d", code)
	}
}

// TestAuditExpectContinue exercises the informational response emitted by a
// real node HTTP server before it reads a forwarded creation request.
func TestAuditExpectContinue(t *testing.T) {
	fastPool(t)
	head, hs, _ := poolMachine(t, "linux")
	pool := poolServer(t, hs, nil)
	node, ns, _ := poolMachine(t, "darwin")
	if code, body := send(t, node, "POST", "/v1/pool/join", `{"token":"`+issueToken(t, head, pool)+`"}`); code != http.StatusOK {
		t.Fatalf("join = %d %s", code, body)
	}
	stop := serveNode(ns)
	defer stop()
	waitFor(t, "the node's first heartbeat", func() bool { return nodeState(t, head) == "ready" })
	req, err := http.NewRequest("POST", head.URL+"/v1/sandboxes", strings.NewReader(`{"name":"expect-demo","os":"macos"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Expect", "100-continue")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("forwarded create with Expect = %d %s, want 201", resp.StatusCode, body)
	}
	var creates []store.AuditEvent
	for _, e := range auditEvents(t, hs.st) {
		if e.Action == "sandbox.create" {
			creates = append(creates, e)
		}
	}
	if len(creates) != 1 || creates[0].Target != "expect-demo" || creates[0].TokenID != "local" {
		t.Fatalf("forwarded create audit = %+v", creates)
	}
}

func TestAuditUnexposeAlreadyRevoked(t *testing.T) {
	fakeServe(t)
	head, hs, _ := poolMachine(t, "linux")
	t.Cleanup(hs.closeExposes)
	if code, _, body := post(t, head, `{"name":"web"}`); code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	port := app(t)
	e, _ := expose(t, head, fmt.Sprintf(`{"sandbox":"web","port":%d}`, port))
	if code, body := send(t, head, "DELETE", "/v1/access-tokens/"+e.TokenID, ""); code != http.StatusNoContent {
		t.Fatalf("revoke = %d %s", code, body)
	}
	code, body := send(t, head, "DELETE", "/v1/exposes/web/"+strconv.Itoa(port), "")
	var removed Expose
	if code != http.StatusOK || json.Unmarshal([]byte(body), &removed) != nil || removed.TokenID != e.TokenID {
		t.Fatalf("unexpose = %d %s, want the original expose identity", code, body)
	}
	var revokes []store.AuditEvent
	list := auditEvents(t, hs.st)
	for _, ev := range list {
		if ev.Action == "token.revoke" && ev.Target == e.TokenID {
			revokes = append(revokes, ev)
		}
	}
	if len(revokes) != 1 {
		t.Fatalf("one token revocation produced %d events: %+v", len(revokes), revokes)
	}
	if list[0].Action != "sandbox.unexpose" || list[0].Target != "web:"+strconv.Itoa(port) {
		t.Fatalf("unexpose event = %+v", list[0])
	}
}
