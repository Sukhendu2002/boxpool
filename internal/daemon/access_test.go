package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"boxpool.run/boxpool/internal/agent"
	"boxpool.run/boxpool/internal/auth"
	"boxpool.run/boxpool/internal/store"
)

func TestAllowed(t *testing.T) {
	cli := store.AccessToken{Scope: "cli"}
	api := store.AccessToken{Scope: "api"}
	web := store.AccessToken{Scope: "cli", Sandbox: "web"}
	expose := store.AccessToken{Scope: "expose"}
	for _, tc := range []struct {
		tok                store.AccessToken
		method, path, body string
		ok                 bool
	}{
		{cli, "GET", "/v1/health", "", true},
		{cli, "GET", "/v1/info", "", true},
		{cli, "GET", "/v1/sandboxes", "", true},
		{api, "POST", "/v1/sandboxes", `{"name":"x"}`, true},
		{cli, "GET", "/v1/sandboxes/web/exec", "", true},
		{cli, "DELETE", "/v1/snapshots/s", "", true},
		{cli, "GET", "/v1/nodes", "", true},
		{cli, "POST", "/v1/nodes", "", false},
		{cli, "GET", "/v1/logs", "", false},
		{cli, "POST", "/v1/tokens", "", false},
		{api, "POST", "/v1/access-tokens", "", false},
		{cli, "GET", "/v1/access-tokens", "", false},
		{cli, "POST", "/v1/pool/promote", "", false},
		{cli, "PUT", "/v1/labels", "", false},
		{cli, "GET", "/v1/health/x", "", false},

		{web, "GET", "/v1/info", "", true},
		{web, "GET", "/v1/sandboxes/web", "", true},
		{web, "GET", "/v1/sandboxes/web/files", "", true},
		{web, "POST", "/v1/sandboxes", `{"name":"web","cpu":2}`, true},
		{web, "POST", "/v1/sandboxes", `{"name":"web","from":"private-backup"}`, false},
		{web, "POST", "/v1/sandboxes", `{"name":"web","From":"private-backup"}`, false},
		{web, "POST", "/v1/sandboxes", `{"name":"web","\u0066rom":"private-backup"}`, false},
		{web, "POST", "/v1/sandboxes", `{"name":"web","from":"","from":"private-backup"}`, false},
		{web, "POST", "/v1/sandboxes", `{"name":"web","from":"private-backup","from":""}`, true},
		{web, "POST", "/v1/sandboxes", `{"name":"web","from":null}`, true},
		{cli, "POST", "/v1/sandboxes", `{"name":"web","from":"private-backup"}`, true},
		{web, "POST", "/v1/sandboxes", `{"name":"other"}`, false},
		{web, "POST", "/v1/sandboxes", `{}`, false},
		{web, "POST", "/v1/sandboxes", `nonsense`, false},
		{web, "GET", "/v1/sandboxes", "", false},
		{web, "GET", "/v1/sandboxes/other/exec", "", false},
		{web, "GET", "/v1/sandboxes/we%62", "", true},
		{web, "GET", "/v1/snapshots", "", false},
		{web, "GET", "/v1/nodes", "", false},

		{expose, "GET", "/v1/health", "", false},
		{expose, "GET", "/v1/sandboxes", "", false},
		{expose, "POST", "/v1/sandboxes", `{"name":"x"}`, false},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		err := allowed(tc.tok, r)
		if (err == nil) != tc.ok {
			t.Errorf("%s token for %q: %s %s = %v, want allowed %v", tc.tok.Scope, tc.tok.Sandbox, tc.method, tc.path, err, tc.ok)
		}
		// The handler must still get the whole body.
		if b, _ := io.ReadAll(r.Body); string(b) != tc.body {
			t.Errorf("%s %s: body after the check = %q, want %q", tc.method, tc.path, b, tc.body)
		}
	}
}

// Snapshot imports refer to a global namespace, outside a bound token's sandbox.
func TestAccessTokenSnapshotImport(t *testing.T) {
	local, d, st := newAPIStore(t)
	_, private, _ := post(t, local, `{"name":"private"}`)
	d.mu.Lock()
	d.disk[private.ID] = "private credentials"
	d.mu.Unlock()
	if code, _, out := postSnapshot(t, local, "private", "private-backup"); code != http.StatusCreated {
		t.Fatalf("snapshot = %d %s", code, out)
	}
	s := newServer(Info{StateDir: filepath.Dir(d.sock)}, st, d)
	for _, scope := range []string{"cli", "api"} {
		t.Run(scope, func(t *testing.T) {
			tok := auth.NewAccessToken([32]byte{})
			rec := store.AccessToken{Hash: tok.Hash(), Scope: scope, Sandbox: "web", CreatedAt: time.Now()}
			if err := st.CreateAccessToken(context.Background(), rec); err != nil {
				t.Fatal(err)
			}
			request := func(body string) *httptest.ResponseRecorder {
				t.Helper()
				r := httptest.NewRequest("POST", "/v1/sandboxes", strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer "+tok.String())
				w := httptest.NewRecorder()
				s.remote(s.handler()).ServeHTTP(w, r)
				return w
			}
			w := request(`{"name":"web","from":"private-backup"}`)
			if w.Code != http.StatusForbidden {
				t.Fatalf("bound snapshot import = %d %s", w.Code, w.Body.String())
			}
			if _, err := st.GetByName(context.Background(), "web"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("rejected import created web: %v", err)
			}
			if w = request(`{"name":"web"}`); w.Code != http.StatusCreated {
				t.Fatalf("bound create = %d %s", w.Code, w.Body.String())
			}
			if w = request(`{"name":"web"}`); w.Code != http.StatusOK {
				t.Fatalf("bound start = %d %s", w.Code, w.Body.String())
			}
			// A token without a sandbox restriction can still clone snapshots.
			rec.Sandbox = ""
			if err := st.RevokeAccessToken(context.Background(), rec.ID()); err != nil {
				t.Fatal(err)
			}
			if err := st.CreateAccessToken(context.Background(), rec); err != nil {
				t.Fatal(err)
			}
			w = request(`{"name":"copy-` + scope + `","from":"private-backup"}`)
			if w.Code != http.StatusCreated {
				t.Fatalf("unbound snapshot import = %d %s", w.Code, w.Body.String())
			}
			copy, err := st.GetByName(context.Background(), "copy-"+scope)
			if err != nil {
				t.Fatal(err)
			}
			d.mu.Lock()
			contents := d.disk[copy.ID]
			d.mu.Unlock()
			if contents != "private credentials" {
				t.Fatalf("clone contents = %q", contents)
			}
			if code, out := call(t, local, "DELETE", "/v1/sandboxes/web?force=1"); code != http.StatusOK {
				t.Fatalf("remove web = %d %s", code, out)
			}
		})
	}
}

type accessBody struct {
	io.ReadCloser
	read func()
}

func (b *accessBody) Read(p []byte) (int, error) {
	if b.read != nil {
		b.read()
		b.read = nil
	}
	return b.ReadCloser.Read(p)
}

// A token may be revoked while allowed reads the scoped create request, even
// before the next periodic check. It must be checked again before dispatch.
func TestAccessTokenRevokedDuringAuthorization(t *testing.T) {
	old := revokeCheck
	revokeCheck = time.Hour
	t.Cleanup(func() { revokeCheck = old })
	_, s := machine(t, "linux")
	tok := auth.NewAccessToken([32]byte{})
	rec := store.AccessToken{Hash: tok.Hash(), Scope: "cli", Sandbox: "web", CreatedAt: time.Now()}
	if err := s.st.CreateAccessToken(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/v1/sandboxes", strings.NewReader(`{"name":"web"}`))
	r.Header.Set("Authorization", "Bearer "+tok.String())
	r.Body = &accessBody{ReadCloser: r.Body, read: func() {
		if err := s.st.RevokeAccessToken(context.Background(), rec.ID()); err != nil {
			t.Fatal(err)
		}
	}}
	w := httptest.NewRecorder()
	s.remote(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("revoked request reached the API")
	})).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("revoked upload = %d %s", w.Code, w.Body.String())
	}
}

// Use a real TLS connection: cancelling a context alone cannot unblock a
// net/http request body read while the client withholds the rest of its upload.
func TestAccessTokenEndsUpload(t *testing.T) {
	old := revokeCheck
	revokeCheck = 20 * time.Millisecond
	t.Cleanup(func() { revokeCheck = old })
	for _, action := range []string{"revoke", "expire"} {
		t.Run(action, func(t *testing.T) {
			_, s := machine(t, "linux")
			reading := make(chan struct{})
			pool := poolServer(t, s, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					r.Body = &accessBody{ReadCloser: r.Body, read: func() { close(reading) }}
					next.ServeHTTP(w, r)
				})
			})
			ca, _, err := auth.LoadOrCreateCA(s.info.StateDir)
			if err != nil {
				t.Fatal(err)
			}
			tok := auth.NewAccessToken(sha256.Sum256(ca.Raw))
			rec := store.AccessToken{Hash: tok.Hash(), Scope: "cli", Sandbox: "web", CreatedAt: time.Now()}
			if action == "expire" {
				rec.ExpiresAt = time.Now().Add(time.Second)
			}
			if err := s.st.CreateAccessToken(context.Background(), rec); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			reader, writer := io.Pipe()
			defer writer.Close()
			defer reader.Close()
			r, err := http.NewRequestWithContext(ctx, "POST", pool.URL+"/v1/sandboxes", reader)
			if err != nil {
				t.Fatal(err)
			}
			r.Header.Set("Authorization", "Bearer "+tok.String())
			hc := &http.Client{Transport: &http.Transport{TLSClientConfig: tok.TLS()}}
			defer hc.CloseIdleConnections()
			type result struct {
				status int
				body   string
				err    error
			}
			finished := make(chan result, 1)
			go func() {
				resp, err := hc.Do(r)
				if err != nil {
					finished <- result{err: err}
					return
				}
				defer resp.Body.Close()
				body, err := io.ReadAll(resp.Body)
				finished <- result{status: resp.StatusCode, body: string(body), err: err}
			}()
			select {
			case <-reading:
			case <-ctx.Done():
				t.Fatal("server did not start reading upload")
			}
			if action == "revoke" {
				if err := s.st.RevokeAccessToken(ctx, rec.ID()); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case got := <-finished:
				if got.err != nil || got.status != http.StatusUnauthorized {
					t.Fatalf("%s during upload = %+v", action, got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("upload outlived its revoked or expired token by 5 seconds")
			}
			if _, err := s.st.GetByName(ctx, "web"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unauthorized upload created web: %v", err)
			}
		})
	}
}

// TestRemoteAccess reaches a head's API on its pool port with access
// tokens, as boxpool login does.
func TestRemoteAccess(t *testing.T) {
	head, s := machine(t, "linux")
	pool := poolServer(t, s, nil)
	create := func(body string) AccessToken {
		t.Helper()
		code, out := send(t, head, "POST", "/v1/access-tokens", body)
		var tok AccessToken
		if err := json.Unmarshal([]byte(out), &tok); code != http.StatusCreated || err != nil {
			t.Fatalf("create %s = %d %s", body, code, out)
		}
		return tok
	}
	call := func(token, method, path, body string) (int, string) {
		t.Helper()
		tok, err := auth.ParseAccessToken(token)
		if err != nil {
			t.Fatal(err)
		}
		hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: tok.TLS()}}
		defer hc.CloseIdleConnections()
		req, _ := http.NewRequest(method, pool.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := hc.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	cli := create(`{"scope":"cli"}`)
	expose := create(`{"scope":"expose"}`)
	if len(cli.ID) != store.AccessIDLen || !cli.ExpiresAt.IsZero() {
		t.Errorf("token = %+v, want a %d-character ID and no expiry", cli, store.AccessIDLen)
	}
	if code, out := call(cli.Token, "GET", "/v1/nodes", ""); code != http.StatusOK || !strings.Contains(out, `"role":"head"`) {
		t.Errorf("cli ls --nodes = %d %s", code, out)
	}
	if code, out := call(cli.Token, "POST", "/v1/access-tokens", `{"scope":"cli"}`); code != http.StatusForbidden {
		t.Errorf("cli token creating a token = %d %s, want 403", code, out)
	}
	if code, out := call(expose.Token, "POST", "/v1/sandboxes", `{"name":"x"}`); code != http.StatusForbidden || !strings.Contains(out, "only opens exposed URLs") {
		t.Errorf("expose token creating a sandbox = %d %s, want 403", code, out)
	}
	pinned, _ := auth.ParseAccessToken(cli.Token)
	if code, out := call(auth.NewAccessToken(pinned.CA).String(), "GET", "/v1/nodes", ""); code != http.StatusUnauthorized {
		t.Errorf("a wrong secret = %d %s, want 401", code, out)
	}
	// A token for another head's CA does not trust this one.
	if code, out := call(auth.NewAccessToken(sha256.Sum256([]byte("other"))).String(), "GET", "/v1/health", ""); code != 0 || !strings.Contains(out, "does not match") {
		t.Errorf("a token pinning another CA = %d %s, want a TLS error", code, out)
	}

	var list []AccessToken
	_, out := send(t, head, "GET", "/v1/access-tokens", "")
	if err := json.Unmarshal([]byte(out), &list); err != nil || len(list) != 2 || list[0].ID != cli.ID || list[0].Token != "" || list[0].LastUsed.IsZero() {
		t.Errorf("token list = %s, want both, without secrets, the cli token used", out)
	}
	if code, out := send(t, head, "DELETE", "/v1/access-tokens/"+cli.ID, ""); code != http.StatusNoContent {
		t.Fatalf("revoke = %d %s", code, out)
	}
	if code, out := call(cli.Token, "GET", "/v1/nodes", ""); code != http.StatusUnauthorized || !strings.Contains(out, "invalid or revoked") {
		t.Errorf("a revoked token = %d %s, want 401", code, out)
	}
	if code, out := send(t, head, "DELETE", "/v1/access-tokens/"+cli.ID, ""); code != http.StatusNotFound {
		t.Errorf("second revoke = %d %s, want 404", code, out)
	}

	brief := create(`{"scope":"api","ttl_ns":1}`)
	if code, out := call(brief.Token, "GET", "/v1/nodes", ""); code != http.StatusUnauthorized || !strings.Contains(out, "expired") {
		t.Errorf("an expired token = %d %s, want 401", code, out)
	}
	for _, body := range []string{`{"scope":"admin"}`, `{"scope":"cli","sandbox":"Bad Name"}`, `{"scope":"cli","ttl_ns":-1}`} {
		if code, out := send(t, head, "POST", "/v1/access-tokens", body); code != http.StatusBadRequest {
			t.Errorf("create %s = %d %s, want 400", body, code, out)
		}
	}
}

// TestRevokeEndsSessions checks that a request still running when its
// token is revoked, such as a shell, ends within revokeCheck.
func TestRevokeEndsSessions(t *testing.T) {
	old := revokeCheck
	revokeCheck = 20 * time.Millisecond
	t.Cleanup(func() { revokeCheck = old })
	_, s := machine(t, "linux")
	tok := auth.NewAccessToken([32]byte{})
	rec := store.AccessToken{Hash: tok.Hash(), Scope: "cli", CreatedAt: time.Now()}
	if err := s.st.CreateAccessToken(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	started, ended := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(s.remote(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("the token reached the API handler")
		}
		close(started)
		<-r.Context().Done()
		close(ended)
	})))
	t.Cleanup(srv.Close)
	go func() {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/sandboxes/web/exec", nil)
		req.Header.Set("Authorization", "Bearer "+tok.String())
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	<-started
	select {
	case <-ended:
		t.Fatal("the session ended before its token was revoked")
	case <-time.After(5 * revokeCheck):
	}
	if err := s.st.RevokeAccessToken(context.Background(), rec.ID()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the session outlived its revoked token by 5 seconds")
	}
}

func TestRevokeEndsExec(t *testing.T) {
	old := revokeCheck
	revokeCheck = 20 * time.Millisecond
	t.Cleanup(func() { revokeCheck = old })
	local, d, st := newAPIStore(t)
	post(t, local, `{"name":"web"}`)
	s := newServer(Info{StateDir: filepath.Dir(d.sock)}, st, d)
	pool := poolServer(t, s, nil)
	code, body := send(t, local, "POST", "/v1/access-tokens", `{"scope":"cli","sandbox":"web"}`)
	var access AccessToken
	if err := json.Unmarshal([]byte(body), &access); err != nil || code != http.StatusCreated {
		t.Fatalf("create token = %d %s: %v", code, body, err)
	}
	tok, err := auth.ParseAccessToken(access.Token)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: tok.TLS()}}
	defer hc.CloseIdleConnections()
	conn, _, err := websocket.Dial(ctx, pool.URL+"/v1/sandboxes/web/exec", &websocket.DialOptions{
		HTTPClient: hc, HTTPHeader: http.Header{"Authorization": {"Bearer " + access.Token}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	session, err := agent.Start(ctx, conn, agent.ExecRequest{Argv: []string{"sleep", "30"}}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RevokeAccessToken(ctx, access.ID); err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	go func() { _, err := session.Wait(); ended <- err }()
	select {
	case err := <-ended:
		if err == nil {
			t.Fatal("revoked exec completed normally")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("exec outlived its revoked token by 5 seconds")
	}
}
