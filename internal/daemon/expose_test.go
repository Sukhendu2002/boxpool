//go:build linux || darwin

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// app is a sandbox's web app. The fake driver's agent runs on this host, so
// its loopback is the sandbox's: it echoes the request, and messages on
// /ws.
func app(t *testing.T) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws" {
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer c.CloseNow()
			typ, msg, err := c.Read(r.Context())
			if err == nil {
				c.Write(r.Context(), typ, append([]byte("echo "), msg...))
			}
			c.Close(websocket.StatusNormalClosure, "")
			return
		}
		if r.URL.Path == "/planted" { // what a sandbox record looks like
			fmt.Fprint(w, `{"id":"planted","name":"planted","state":"running"}`)
			return
		}
		fmt.Fprintf(w, "%s %s?%s host=%s cookie=%q", r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Host, r.Header.Get("Cookie"))
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().(*net.TCPAddr).Port
}

// fakeServe stands in for tailscale serve, recording the ports it serves.
func fakeServe(t *testing.T) map[int]bool {
	served := map[int]bool{}
	oldServe, oldUnserve := serveHTTPS, unserveHTTPS
	serveHTTPS = func(_ context.Context, port int) (string, error) {
		served[port] = true
		return "https://head.test:" + strconv.Itoa(port) + "/", nil
	}
	unserveHTTPS = func(_ context.Context, port int) error { delete(served, port); return nil }
	t.Cleanup(func() { serveHTTPS, unserveHTTPS = oldServe, oldUnserve })
	return served
}

// expose exposes a port and returns the answer and the loopback URL that
// tailscale serve would send it to.
func expose(t *testing.T, head *httptest.Server, body string) (Expose, string) {
	t.Helper()
	code, out := send(t, head, "POST", "/v1/exposes", body)
	var e Expose
	if code != http.StatusCreated || json.Unmarshal([]byte(out), &e) != nil {
		t.Fatalf("expose %s = %d %s", body, code, out)
	}
	port := strings.TrimSuffix(strings.Split(strings.TrimPrefix(e.URL, "https://head.test:"), "?")[0], "/")
	return e, "http://127.0.0.1:" + port
}

func get(t *testing.T, url string, cookies ...*http.Cookie) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := http.DefaultTransport.RoundTrip(req) // no redirects
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestExpose(t *testing.T) {
	served := fakeServe(t)
	head, _, _ := poolMachine(t, "linux")
	port := app(t)
	p := strconv.Itoa(port)
	post(t, head, `{"name":"web"}`)
	post(t, head, `{"name":"api"}`)

	e, base := expose(t, head, `{"sandbox":"web","port":`+p+`}`)
	token := strings.SplitN(e.URL, "?"+tokenParam+"=", 2)[1]
	if e.Sandbox != "web" || e.Port != port || e.Public || e.TokenID == "" || len(served) != 1 {
		t.Fatalf("expose = %+v; served %v", e, served)
	}

	// No token, or a wrong one: 401.
	if resp, body := get(t, base+"/hello"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("without a token = %d %s", resp.StatusCode, body)
	}
	if resp, _ := get(t, base+"/hello?"+tokenParam+"=bpat1_nope"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("with a bad token = %d", resp.StatusCode)
	}
	for _, req := range []string{`{"scope":"expose","sandbox":"api"}`, `{"scope":"cli"}`} {
		_, out := send(t, head, "POST", "/v1/access-tokens", req)
		var other AccessToken
		json.Unmarshal([]byte(out), &other)
		if resp, _ := get(t, base+"/?"+tokenParam+"="+other.Token); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("with token %s = %d", req, resp.StatusCode)
		}
	}

	// The link's token becomes a cookie, and leaves the address.
	resp, _ := get(t, base+"//evil.example/a%20b?"+tokenParam+"="+token+"&x=1")
	cookies := resp.Cookies()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/evil.example/a%20b?x=1" || len(cookies) != 1 || !cookies[0].HttpOnly || !cookies[0].Secure {
		t.Fatalf("token link = %d %q %v", resp.StatusCode, resp.Header.Get("Location"), cookies)
	}
	resp, body := get(t, base+"/a%2Fb?x=1", cookies[0], &http.Cookie{Name: "app", Value: "1"}, &http.Cookie{Name: cookiePrefix + "1", Value: "other"})
	if want := `GET /a%2Fb?x=1 host=localhost:` + p + ` cookie="app=1"`; resp.StatusCode != http.StatusOK || body != want {
		t.Errorf("with the cookie = %d %q, want %q", resp.StatusCode, body, want)
	}

	// WebSockets, as a dev server's hot reload uses.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Cookie": {cookies[0].String()}}})
	if err != nil {
		t.Fatal(err)
	}
	ws.Write(ctx, websocket.MessageText, []byte("hi"))
	if _, msg, err := ws.Read(ctx); err != nil || string(msg) != "echo hi" {
		t.Errorf("websocket = %q, %v", msg, err)
	}
	ws.CloseNow()

	if code, body := send(t, head, "POST", "/v1/exposes", `{"sandbox":"web","port":`+p+`}`); code != http.StatusConflict {
		t.Errorf("expose twice = %d %s", code, body)
	}
	if code, body := send(t, head, "POST", "/v1/exposes", `{"sandbox":"nope","port":`+p+`}`); code != http.StatusNotFound {
		t.Errorf("expose a missing sandbox = %d %s", code, body)
	}

	// --public needs no token.
	pub, pubBase := expose(t, head, `{"sandbox":"api","port":`+p+`,"public":true}`)
	if resp, body := get(t, pubBase+"/"); pub.TokenID != "" || strings.Contains(pub.URL, "?") || resp.StatusCode != http.StatusOK {
		t.Errorf("public = %+v; GET = %d %s", pub, resp.StatusCode, body)
	}

	var list []Expose
	_, out := send(t, head, "GET", "/v1/exposes", "")
	if json.Unmarshal([]byte(out), &list); len(list) != 2 || strings.Contains(list[0].URL, "?") {
		t.Errorf("exposes = %s", out)
	}

	// rm stops the URL and revokes its token.
	if code, body := send(t, head, "DELETE", "/v1/exposes/web/"+p, ""); code != http.StatusOK {
		t.Fatalf("expose rm = %d %s", code, body)
	}
	if _, err := http.Get(base + "/"); err == nil {
		t.Error("the URL still answers after expose rm")
	}
	if _, out := send(t, head, "GET", "/v1/access-tokens", ""); strings.Contains(out, e.TokenID) {
		t.Errorf("token %s outlives expose rm: %s", e.TokenID, out)
	}
	if len(served) != 1 {
		t.Errorf("tailscale serves %v after rm, want only the public one", served)
	}
	if code, _ := send(t, head, "DELETE", "/v1/exposes/web/"+p, ""); code != http.StatusNotFound {
		t.Errorf("expose rm twice = %d", code)
	}
}

// TestExposeOnNode exposes a port of a sandbox on a node: the head serves
// it through the node's tunnel, WebSockets included.
func TestExposeOnNode(t *testing.T) {
	fakeServe(t)
	fastPool(t)
	head, headServer, headDriver := poolMachine(t, "linux")
	pool := poolServer(t, headServer, nil)
	node, nodeServer, nodeDriver := poolMachine(t, "darwin")
	if code, body := send(t, node, "POST", "/v1/pool/join", `{"token":"`+issueToken(t, head, pool)+`"}`); code != http.StatusOK {
		t.Fatalf("join = %d %s", code, body)
	}
	stop := serveNode(nodeServer)
	defer stop()
	waitFor(t, "the node's first heartbeat", func() bool { return nodeState(t, head) == "ready" })
	if code, _, body := post(t, head, `{"name":"mac","os":"macos"}`); code != http.StatusCreated || len(nodeDriver.spec) != 1 || len(headDriver.spec) != 0 {
		t.Fatalf("create on the node = %d %s", code, body)
	}
	p := strconv.Itoa(app(t))
	e, base := expose(t, head, `{"sandbox":"mac","port":`+p+`,"public":true}`)
	if resp, body := get(t, base+"/x?y=1"); resp.StatusCode != http.StatusOK || body != `GET /x?y=1 host=localhost:`+p+` cookie=""` {
		t.Errorf("GET through the head = %d %q (%+v)", resp.StatusCode, body, e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	ws.Write(ctx, websocket.MessageText, []byte("hi"))
	if _, msg, err := ws.Read(ctx); err != nil || string(msg) != "echo hi" {
		t.Errorf("websocket through the head = %q, %v", msg, err)
	}
	// The node's answer is the app's, not a sandbox record for the head.
	resp, err := http.Post(base+"/planted", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := states(list(t, head)); got != "mac=running" {
		t.Errorf("head lists %s after a POST to the app", got)
	}
	if code, body := send(t, node, "POST", "/v1/exposes", `{"sandbox":"mac","port":`+p+`}`); code != http.StatusConflict {
		t.Errorf("expose on a node = %d %s", code, body)
	}
}
