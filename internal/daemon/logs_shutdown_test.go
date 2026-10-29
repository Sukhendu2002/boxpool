//go:build linux || darwin

package daemon

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/backend"
	"boxpool.run/boxpool/internal/store"
)

func TestRunCancelsLogFollowersBeforeShutdown(t *testing.T) {
	for _, path := range []string{"/v1/logs", "/v1/sandboxes/box/logs"} {
		t.Run(path, func(t *testing.T) {
			dir, err := os.MkdirTemp("", "bp")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			logPath := filepath.Join(dir, LogName)
			if err := os.WriteFile(logPath, []byte("initial\n"), 0600); err != nil {
				t.Fatal(err)
			}
			oldOpen, oldTimeout := openBackend, shutdownTimeout
			defer func() { openBackend, shutdownTimeout = oldOpen, oldTimeout }()
			shutdownTimeout = time.Second
			openBackend = func(ctx context.Context, _ string, _ *Info, st *store.Store) (Driver, error) {
				d := newFakeDriver(t, dir)
				sb, err := st.Create(ctx, store.Sandbox{Backend: d.Name(), Spec: backend.Spec{Name: "box", CPUs: 1, MemoryMB: 512, DiskGB: 1}})
				if err != nil {
					return nil, err
				}
				if _, err := d.Create(ctx, sb.ID, sb.Spec); err != nil {
					return nil, err
				}
				return &consoleDriver{d, logPath}, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			var runErr error
			go func() {
				runErr = Run(ctx, dir, Info{Backend: "fake", AllowSleep: true})
				close(done)
			}()
			defer func() {
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("daemon did not stop")
				}
			}()
			transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, SocketName))
			}}
			defer transport.CloseIdleConnections()
			hc := &http.Client{Transport: transport, Timeout: 5 * time.Second}
			var resp *http.Response
			for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
				resp, err = hc.Get("http://daemon" + path + "?follow=true")
				if err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal(err)
				}
			}
			defer resp.Body.Close()
			b := make([]byte, len("initial\n"))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("follow returned %s", resp.Status)
			}
			if _, err := io.ReadFull(resp.Body, b); err != nil || string(b) != "initial\n" {
				t.Fatalf("initial log = %q, %v", b, err)
			}
			cancel()
			select {
			case <-done:
				if runErr != nil {
					t.Errorf("normal shutdown with an active log follower failed: %v", runErr)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("log follower blocked shutdown")
			}
			if _, err := io.ReadAll(resp.Body); err != nil {
				t.Errorf("local follower did not end cleanly: %v", err)
			}
		})
	}
}
