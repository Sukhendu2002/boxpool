//go:build linux || darwin

package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/backend"
	"boxpool.run/boxpool/internal/store"
)

// delayedCreate models filesystem work that cannot be canceled immediately,
// followed by a failure whose handler must roll back its store record.
type delayedCreate struct {
	*fakeDriver
	entered  chan string
	canceled chan struct{}
	release  chan struct{}
}

func (d *delayedCreate) Create(ctx context.Context, id string, _ backend.Spec) (backend.Sandbox, error) {
	d.entered <- id
	<-ctx.Done()
	close(d.canceled)
	<-d.release
	return backend.Sandbox{}, errors.New("injected create failure")
}

type shutdownWarnings struct {
	slog.Handler
	warnings chan slog.Record
}

func (h shutdownWarnings) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		select {
		case h.warnings <- r.Clone():
		default:
		}
	}
	return h.Handler.Handle(ctx, r)
}

func TestRunWaitsForRollbackAfterShutdownTimeout(t *testing.T) {
	tmp, err := os.MkdirTemp("", "bp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	d := &delayedCreate{fakeDriver: newFakeDriver(t, tmp), entered: make(chan string, 1), canceled: make(chan struct{}), release: make(chan struct{})}
	oldOpen, oldTimeout, oldLogger := openBackend, shutdownTimeout, slog.Default()
	defer func() { openBackend = oldOpen; shutdownTimeout = oldTimeout; slog.SetDefault(oldLogger) }()
	shutdownTimeout = 30 * time.Millisecond
	opened := make(chan *store.Store, 1)
	openBackend = func(_ context.Context, _ string, _ *Info, st *store.Store) (Driver, error) {
		opened <- st
		return d, nil
	}
	warnings := make(chan slog.Record, 4)
	slog.SetDefault(slog.New(shutdownWarnings{Handler: slog.NewTextHandler(io.Discard, nil), warnings: warnings}))
	dir := filepath.Join(tmp, "state")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var runErr error
	go func() { runErr = Run(ctx, dir, Info{Backend: "fake"}); close(done) }()
	var release sync.Once
	defer func() {
		cancel()
		release.Do(func() { close(d.release) })
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not finish after the create was released")
		}
	}()
	var liveStore *store.Store
	select {
	case liveStore = <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("backend was not opened")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, SocketName))
	}}
	defer transport.CloseIdleConnections()
	hc := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		resp, err := hc.Get("http://daemon/v1/health")
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
	}
	posted := make(chan struct{})
	go func() {
		defer close(posted)
		resp, err := hc.Post("http://daemon/v1/sandboxes", "application/json", strings.NewReader(`{"name":"pending"}`))
		if err == nil {
			resp.Body.Close()
		}
	}()
	var id string
	select {
	case id = <-d.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("create did not start")
	}
	cancel()
	select {
	case <-done:
		t.Fatalf("Run returned before handler rollback: %v", runErr)
	case r := <-warnings:
		if r.Message != "requests still running after shutdown; waiting for cleanup" {
			t.Errorf("warning: %s", r.Message)
		}
		var requests int64
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "requests" {
				requests = a.Value.Int64()
			}
			return true
		})
		if requests != 1 {
			t.Errorf("warning request count = %d, want 1", requests)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no warning for the blocked handler")
	}
	select {
	case <-d.canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("handler context was not canceled")
	}
	select {
	case <-done:
		t.Fatal("Run returned with cleanup still blocked")
	default:
	}
	if sb, err := liveStore.GetByName(context.Background(), "pending"); err != nil || sb.ID != id {
		t.Fatalf("store closed before cleanup: %+v, %v", sb, err)
	}
	if err := Run(ctx, dir, Info{Backend: "fake"}); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("lock released before cleanup: %v", err)
	}
	release.Do(func() { close(d.release) })
	select {
	case <-done:
		if !errors.Is(runErr, context.DeadlineExceeded) {
			t.Errorf("Run after delayed cleanup: %v", runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not finish after rollback")
	}
	select {
	case <-posted:
	case <-time.After(5 * time.Second):
		t.Fatal("POST did not finish")
	}
	st, err := store.Open(filepath.Join(dir, StoreName))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.GetByName(context.Background(), "pending"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("rollback left sandbox record: %v", err)
	}
	if _, err := liveStore.List(context.Background()); err == nil {
		t.Error("Run did not close the store after cleanup")
	}
}
