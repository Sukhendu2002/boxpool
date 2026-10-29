//go:build linux || darwin

package daemon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/store"
)

type warmDriver struct {
	*fakeDriver
	entered, release chan struct{}
	err              error
}

func (d *warmDriver) Warm(context.Context) error {
	close(d.entered)
	<-d.release
	return d.err
}

func TestRunWarmsBeforeServingAndFallsBack(t *testing.T) {
	dir, err := os.MkdirTemp("", "bpwarm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	d := &warmDriver{fakeDriver: newFakeDriver(t, dir), entered: make(chan struct{}), release: make(chan struct{}), err: errors.New("no saved state support")}
	old := openBackend
	t.Cleanup(func() { openBackend = old })
	openBackend = func(context.Context, string, *Info, *store.Store) (Driver, error) { return d, nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, dir, Info{Backend: "fake"}) }()
	var release sync.Once
	t.Cleanup(func() {
		release.Do(func() { close(d.release) })
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	select {
	case <-d.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("warm preparation not called")
	}
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, SocketName))
	}}}
	t.Cleanup(c.CloseIdleConnections)
	health := func(ctx context.Context) error {
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://daemon/v1/health", nil)
		resp, err := c.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return errors.New("not healthy")
			}
		}
		return err
	}
	probe, end := context.WithTimeout(context.Background(), 30*time.Millisecond)
	if err := health(probe); err == nil {
		t.Error("healthy before warm base was ready")
	}
	end()
	release.Do(func() { close(d.release) })
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		probe, end := context.WithTimeout(context.Background(), 100*time.Millisecond)
		err := health(probe)
		end()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("warm failure prevented cold-boot daemon from serving")
		}
	}
}
