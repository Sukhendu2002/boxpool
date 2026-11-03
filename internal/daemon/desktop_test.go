//go:build linux || darwin

package daemon

import (
	"context"
	"fmt"
	"github.com/coder/websocket"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDesktopExposeAuthAndNodeRouting(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(fmt.Sprint("remote=", remote), func(t *testing.T) {
			fakeServe(t)
			fastPool(t)
			head, s, _ := poolMachine(t, "linux")
			t.Cleanup(s.closeExposes)
			osFlag := ""
			if remote {
				pool := poolServer(t, s, nil)
				node, ns, _ := poolMachine(t, "darwin")
				if code, out := send(t, node, "POST", "/v1/pool/join", `{"token":"`+issueToken(t, head, pool)+`"}`); code != 200 {
					t.Fatalf("join: %d %s", code, out)
				}
				stop := serveNode(ns)
				defer stop()
				waitFor(t, "node ready", func() bool { return nodeState(t, head) == "ready" })
				osFlag = `,"os":"macos"`
			}
			if code, _, out := post(t, head, `{"name":"screen","image":"desktop"`+osFlag+`}`); code != 201 {
				t.Fatalf("create: %d %s", code, out)
			}
			// Starting on a base-image test agent reports a useful error through either route.
			if code, out := send(t, head, "POST", "/v1/sandboxes/screen/desktop", ""); code != 409 || !strings.Contains(out, "--image desktop") {
				t.Fatalf("start: %d %s", code, out)
			}
			e, base := expose(t, head, `{"sandbox":"screen","desktop":true}`)
			if e.Public || e.Port != DesktopPort {
				t.Fatalf("desktop expose: %+v", e)
			}
			for _, path := range []string{"/", "/viewer.js", "/vendor/core/rfb.js", "/ws", "/control"} {
				if resp, _ := get(t, base+path); resp.StatusCode != 401 {
					t.Fatalf("unauthenticated %s: %d", path, resp.StatusCode)
				}
			}
			resp, _ := get(t, base+"/?"+strings.SplitN(e.URL, "?", 2)[1])
			cookies := resp.Cookies()
			if resp.StatusCode != 303 || len(cookies) != 1 {
				t.Fatalf("login: %d %v", resp.StatusCode, cookies)
			}
			for _, path := range []string{"/", "/viewer.js", "/vendor/core/rfb.js"} {
				resp, body := get(t, base+path, cookies...)
				if resp.StatusCode != 200 || len(body) == 0 {
					t.Fatalf("asset %s: %d %s", path, resp.StatusCode, body)
				}
			}
			req, _ := http.NewRequest("POST", base+"/control", strings.NewReader(`{"human_control":true}`))
			req.AddCookie(cookies[0])
			req.Header.Set("Origin", "https://evil.test")
			foreign, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			foreign.Body.Close()
			if foreign.StatusCode != 403 {
				t.Fatalf("foreign control: %d", foreign.StatusCode)
			}
			if code, out := send(t, head, "DELETE", "/v1/access-tokens/"+e.TokenID, ""); code != 204 {
				t.Fatalf("revoke: %d %s", code, out)
			}
			if resp, _ := get(t, base+"/", cookies...); resp.StatusCode != 401 {
				t.Fatalf("revoked viewer: %d", resp.StatusCode)
			}
			if code, out := send(t, head, "DELETE", "/v1/exposes/screen/"+strconv.Itoa(DesktopPort), ""); code != 200 {
				t.Fatalf("rm: %d %s", code, out)
			}
		})
	}
}

func TestDesktopImageValidation(t *testing.T) {
	srv, d := newAPI(t)
	for _, body := range []string{`{"image":"unknown"}`, `{"image":"desktop","from":"snapshot"}`} {
		if code, _, out := post(t, srv, body); code != 400 {
			t.Fatalf("%s: %d %s", body, code, out)
		}
	}
	code, sb, out := post(t, srv, `{"name":"screen","image":"desktop"}`)
	if code != 201 {
		t.Fatalf("create: %d %s", code, out)
	}
	if d.spec[sb.ID].Image != "desktop" {
		t.Fatalf("driver image: %+v", d.spec[sb.ID])
	}
	if got, err := d.Dial(context.Background(), sb.ID); err != nil {
		t.Fatal(err)
	} else {
		got.Close()
	}
	if code, _, out := post(t, srv, `{"name":"screen","image":"base"}`); code != 409 {
		t.Fatalf("reattach mismatch: %d %s", code, out)
	}
	fakeServe(t)
	head, s, _ := poolMachine(t, "linux")
	defer s.closeExposes()
	post(t, head, `{"name":"screen"}`)
	for _, body := range []string{`{"sandbox":"screen","port":0}`, `{"sandbox":"screen","desktop":true,"public":true}`, `{"sandbox":"screen","desktop":true,"port":5900}`} {
		code, out := send(t, head, "POST", "/v1/exposes", body)
		if code != 400 {
			t.Fatalf("expose %s: %d %s", body, code, out)
		}
	}
}

func TestDesktopRevocationClosesActiveViewer(t *testing.T) {
	fakeServe(t)
	old := revokeCheck
	revokeCheck = 20 * time.Millisecond
	defer func() { revokeCheck = old }()
	head, s, d := poolMachine(t, "linux")
	defer s.closeExposes()
	post(t, head, `{"name":"screen"}`)
	// Replace the fake agent's socket with a binary echo desktop service so
	// this test exercises authenticated, long-lived upgrades through the proxy.
	socket := filepath.Join(s.info.StateDir, "desktop-agent.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	up := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		for {
			typ, b, err := ws.Read(r.Context())
			if err != nil {
				return
			}
			if ws.Write(r.Context(), typ, b) != nil {
				return
			}
		}
	})}
	go up.Serve(l)
	defer up.Close()
	d.mu.Lock()
	d.sock = socket
	d.mu.Unlock()
	e, base := expose(t, head, `{"sandbox":"screen","desktop":true}`)
	login, _ := get(t, base+"/?"+strings.SplitN(e.URL, "?", 2)[1])
	cookies := login.Cookies()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, base+"/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Cookie": {cookies[0].String()}, "Origin": {strings.SplitN(e.URL, "/?", 2)[0]}}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	ws.Write(ctx, websocket.MessageBinary, []byte("desktop"))
	if _, b, err := ws.Read(ctx); err != nil || string(b) != "desktop" {
		t.Fatalf("stream: %q %v", b, err)
	}
	if code, out := send(t, head, "DELETE", "/v1/access-tokens/"+e.TokenID, ""); code != 204 {
		t.Fatalf("revoke: %d %s", code, out)
	}
	if _, _, err := ws.Read(ctx); err == nil || ctx.Err() != nil {
		t.Fatalf("viewer survived revocation: %v", err)
	}
}
