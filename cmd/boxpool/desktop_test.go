//go:build linux || darwin

package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"boxpool.run/boxpool/internal/daemon"
)

func TestDesktopCLI(t *testing.T) {
	dir, err := os.MkdirTemp("", "bp-desktop")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	l, err := net.Listen("unix", filepath.Join(dir, "boxpool.sock"))
	if err != nil {
		t.Fatal(err)
	}
	calls := make(chan string, 10)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			w.Write([]byte(`{"OK":true}`))
			return
		}
		calls <- r.Method + " " + r.URL.Path
		switch r.URL.Path {
		case "/v1/sandboxes/box/desktop":
			w.Write([]byte(`{"display":":0","vnc":"unix:/run/boxpool/desktop.vnc"}`))
		case "/v1/exposes":
			var req daemon.ExposeRequest
			json.NewDecoder(r.Body).Decode(&req)
			if req.Sandbox != "box" || !req.Desktop || req.Public || req.Port != 0 {
				t.Errorf("request: %+v", req)
			}
			w.WriteHeader(201)
			w.Write([]byte(`{"sandbox":"box","url":"https://head.test/?boxpool_token=secret"}`))
		default:
			w.Write([]byte(`{}`))
		}
	})}
	go srv.Serve(l)
	defer srv.Close()
	code, out, errOut := cli(t, dir, "", "desktop", "--no-open", "box")
	if code != 0 || out != "https://head.test/?boxpool_token=secret\n" || !strings.Contains(errOut, "desktop rm box") {
		t.Fatalf("desktop: %d %s %s", code, out, errOut)
	}
	if got := <-calls; got != "POST /v1/sandboxes/box/desktop" {
		t.Fatal(got)
	}
	if got := <-calls; got != "POST /v1/exposes" {
		t.Fatal(got)
	}
	if code, _, errOut := cli(t, dir, "", "desktop", "rm", "box"); code != 0 || errOut != "" {
		t.Fatalf("remove: %d %s", code, errOut)
	}
	if got := <-calls; got != "DELETE /v1/exposes/box/0" {
		t.Fatal(got)
	}
	for _, args := range [][]string{{"desktop"}, {"desktop", "--bad"}, {"desktop", "box", "extra"}, {"desktop", "rm"}, {"sh", "--image", "unknown"}, {"sh", "--image", "desktop", "--from", "snap"}, {"sh", "--image", "desktop", "box"}} {
		if code, _, errOut := cli(t, dir, "", args...); code != 2 {
			t.Fatalf("%v: %d %s", args, code, errOut)
		}
	}
}
