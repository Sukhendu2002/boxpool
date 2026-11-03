package agent

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestDesktopViewerControl(t *testing.T) {
	dir, err := os.MkdirTemp("", "bpd")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "vnc")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := newDesktop(ctx)
	d.socket = socket
	var mu sync.Mutex
	var inputs []bool
	d.input = func(_ context.Context, human bool) error {
		mu.Lock()
		defer mu.Unlock()
		inputs = append(inputs, human)
		return nil
	}
	mux := http.NewServeMux()
	d.routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	request := func(body, origin string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/desktop/control", strings.NewReader(body))
		req.Header.Set("Origin", origin)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if got := request(`{"human_control":true}`, ""); got != 409 {
		t.Fatalf("no viewer: %d", got)
	}
	wsctx, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	ws, _, err := websocket.Dial(wsctx, srv.URL+"/v1/desktop/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	ws.Write(wsctx, websocket.MessageBinary, []byte("RFB echo"))
	if _, b, err := ws.Read(wsctx); err != nil || string(b) != "RFB echo" {
		t.Fatalf("stream: %q %v", b, err)
	}
	other, resp, err := websocket.Dial(wsctx, srv.URL+"/v1/desktop/ws", nil)
	if other != nil {
		other.CloseNow()
	}
	if err == nil || resp.StatusCode != 409 {
		t.Fatalf("second viewer: %v %v", resp, err)
	}
	if got := request(`{"human_control":true}`, "https://evil.test"); got != 403 {
		t.Fatalf("foreign control: %d", got)
	}
	if got := request(`bad`, srv.URL); got != 400 {
		t.Fatalf("bad control: %d", got)
	}
	for _, human := range []bool{true, false, true} {
		body := `{"human_control":false}`
		if human {
			body = `{"human_control":true}`
		}
		if got := request(body, srv.URL); got != 200 {
			t.Fatalf("control: %d", got)
		}
		resp, err := http.Get(srv.URL + "/v1/desktop")
		if err != nil {
			t.Fatal(err)
		}
		var status Desktop
		json.NewDecoder(resp.Body).Decode(&status)
		resp.Body.Close()
		if status.HumanControl != human || status.Display != ":0" || status.VNC != "unix:"+socket {
			t.Fatalf("status: %+v", status)
		}
	}
	ws.CloseNow()
	deadline := time.Now().Add(time.Second)
	for {
		d.mu.Lock()
		active := d.viewer
		d.mu.Unlock()
		if !active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("disconnect did not release control")
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(inputs) != 5 || inputs[0] || !inputs[1] || inputs[2] || !inputs[3] || inputs[4] {
		t.Fatalf("input policy transitions: %v", inputs)
	}
}

func TestDesktopAssetsAndMissingImage(t *testing.T) {
	d := newDesktop(context.Background())
	d.command = filepath.Join(t.TempDir(), "missing")
	mux := http.NewServeMux()
	d.routes(mux)
	for _, path := range []string{"/v1/desktop/", "/v1/desktop/viewer.js", "/v1/desktop/vendor/core/rfb.js", "/v1/desktop/vendor/vendor/pako/lib/zlib/inflate.js"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || w.Body.Len() == 0 {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/v1/desktop", nil))
	if w.Code != 409 || !strings.Contains(w.Body.String(), "--image desktop") {
		t.Fatalf("missing image: %d %s", w.Code, w.Body)
	}
}

func TestDesktopProcessLifetime(t *testing.T) {
	dir, err := os.MkdirTemp("", "bp-life")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "vnc")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	// The private fake VNC listener lets startup exercise the real supervisor
	// lifecycle without requiring X on the unit-test host.
	go func() {
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			c.Close()
		}
	}()
	script := filepath.Join(dir, "desktop")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntrap 'exit 0' TERM\nwhile :; do sleep 0.05; done\n"), 0755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := newDesktop(ctx)
	d.command = script
	d.socket = socket
	defer d.close()
	for range 2 {
		w := httptest.NewRecorder()
		d.start(w, httptest.NewRequest("POST", "/v1/desktop", nil))
		if w.Code != 200 {
			t.Fatalf("start: %d %s", w.Code, w.Body)
		}
	}
	done := d.done
	cancel()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("desktop outlived agent cancellation")
	}
	// A failed restart after a crash must retire the old cancellation handle;
	// close must never wait on a nil replacement process.
	d.command = filepath.Join(dir, "missing")
	w := httptest.NewRecorder()
	d.start(w, httptest.NewRequest("POST", "/v1/desktop", nil))
	if w.Code != 409 {
		t.Fatalf("restart: %d", w.Code)
	}
}
