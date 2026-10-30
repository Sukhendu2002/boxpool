//go:build linux || darwin

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
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
		if r.URL.Path == "/login" || r.URL.Path == "/ws" {
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "app-secret", Path: "/"})
		}
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
	head, headServer, _ := poolMachine(t, "linux")
	t.Cleanup(headServer.closeExposes)
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
	if want := `GET /a%2Fb?x=1 host=localhost:` + p + ` cookie=""`; resp.StatusCode != http.StatusOK || body != want {
		t.Errorf("with the cookie = %d %q, want %q", resp.StatusCode, body, want)
	}

	// WebSockets, as a dev server's hot reload uses.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Cookie": {cookies[0].String()}, "Origin": {strings.SplitN(e.URL, "/?", 2)[0]}}})
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
	t.Cleanup(headServer.closeExposes)
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
	login, _ := get(t, base+"/login")
	appCookies := login.Cookies()
	if len(appCookies) != 1 || !appCookies[0].HttpOnly || !appCookies[0].Secure || appCookies[0].Value == "app-secret" {
		t.Fatalf("node app cookies = %v", appCookies)
	}
	if _, body := get(t, base+"/", appCookies...); !strings.Contains(body, `cookie="session=app-secret"`) {
		t.Fatalf("node app session did not round-trip: %s", body)
	}
	ws, handshake, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {strings.TrimSuffix(e.URL, "/")}}})
	if err != nil {
		t.Fatal(err)
	}
	if cookies := handshake.Cookies(); len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].Value == "app-secret" {
		t.Fatalf("node websocket cookies = %v", cookies)
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
	if code, body := send(t, node, "GET", "/v1/exposes/check", ""); code != http.StatusConflict {
		t.Errorf("check expose on a node = %d %s", code, body)
	}
	open, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer open.CloseNow()
	if code, body := send(t, head, "DELETE", "/v1/exposes/mac/"+p, ""); code != http.StatusOK {
		t.Fatalf("rm on head = %d %s", code, body)
	}
	if _, _, err := open.Read(ctx); err == nil || ctx.Err() != nil {
		t.Fatalf("node WebSocket not closed promptly on removal: %v", err)
	}
}

func TestExposeCookieIsolation(t *testing.T) {
	fakeServe(t)
	head, s, _ := poolMachine(t, "linux")
	t.Cleanup(s.closeExposes)
	post(t, head, `{"name":"trusted"}`)
	post(t, head, `{"name":"untrusted"}`)
	port := app(t)
	a, aBase := expose(t, head, fmt.Sprintf(`{"sandbox":"trusted","port":%d,"public":true}`, port))
	b, bBase := expose(t, head, fmt.Sprintf(`{"sandbox":"untrusted","port":%d,"public":true}`, port))
	resp, _ := get(t, aBase+"/login")
	cookies := resp.Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].Domain != "" || strings.Contains(cookies[0].Value, "app-secret") {
		t.Fatalf("app cookie was not protected: %v", cookies)
	}
	jar, _ := cookiejar.New(nil)
	aURL, _ := url.Parse(a.URL)
	bURL, _ := url.Parse(b.URL)
	jar.SetCookies(aURL, cookies)
	if _, body := get(t, bBase+"/", jar.Cookies(bURL)...); strings.Contains(body, "app-secret") || !strings.Contains(body, `cookie=""`) {
		t.Fatalf("session leaked to another expose: %s", body)
	}
	if _, body := get(t, aBase+"/", jar.Cookies(aURL)...); !strings.Contains(body, `cookie="session=app-secret"`) {
		t.Fatalf("legitimate session failed: %s", body)
	}
}

func TestExposeRemovalCancelsWebSocket(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprint("shutdown=", shutdown), func(t *testing.T) {
			fakeServe(t)
			head, s, _ := poolMachine(t, "linux")
			t.Cleanup(s.closeExposes)
			post(t, head, `{"name":"web"}`)
			port := app(t)
			_, base := expose(t, head, fmt.Sprintf(`{"sandbox":"web","port":%d,"public":true}`, port))
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/ws", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer c.CloseNow()
			if shutdown {
				s.closeExposes()
			} else {
				if code, out := send(t, head, "DELETE", "/v1/exposes/web/"+strconv.Itoa(port), ""); code != http.StatusOK {
					t.Fatalf("rm = %d %s", code, out)
				}
			}
			_, _, err = c.Read(ctx)
			if err == nil || ctx.Err() != nil {
				t.Fatalf("WebSocket not closed promptly: %v", err)
			}
		})
	}
}

func TestExposeForeignWebSocketOrigin(t *testing.T) {
	fakeServe(t)
	head, s, _ := poolMachine(t, "linux")
	t.Cleanup(s.closeExposes)
	post(t, head, `{"name":"web"}`)
	_, base := expose(t, head, fmt.Sprintf(`{"sandbox":"web","port":%d,"public":true}`, app(t)))
	for _, origin := range []string{"https://evil.example", "null", "https://head.test:1"} {
		c, resp, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(base, "http")+"/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {origin}}})
		if c != nil {
			c.CloseNow()
		}
		if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
			t.Errorf("Origin %s = %v %v", origin, resp, err)
		}
	}
}
