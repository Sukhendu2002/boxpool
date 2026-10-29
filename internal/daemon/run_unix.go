//go:build linux || darwin

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"boxpool.run/boxpool/internal/store"
)

// shutdownTimeout is the HTTP shutdown grace period and the delay before
// warning about handlers that have not finished their cleanup.
var shutdownTimeout = 5 * time.Second

// openBackend is openDriver; tests replace it with a fake.
var openBackend = openDriver

// Run creates the state directory, takes the daemon lock, opens the sandbox
// store and the backend named by info.Backend (on Linux: firecracker or
// docker; on macOS: the Virtualization framework) and serves the API on the
// socket in dir until ctx is cancelled. Only one daemon may run per state
// directory. Shutdown cancels sessions and waits for every handler before
// releasing the store and lock. Cleanup may outlast shutdownTimeout when a
// backend operation cannot finish immediately after cancellation.
func Run(ctx context.Context, dir string, info Info) (err error) {
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	// Anyone who can write here could replace the lock or the socket.
	if fi.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("state directory %s is writable by group or others (mode %v): run chmod go-w on it", dir, fi.Mode().Perm())
	}
	lock, err := os.OpenFile(filepath.Join(dir, LockName), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	defer lock.Close() // releases the flock
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("another daemon is already running with state directory %s", dir)
		}
		return fmt.Errorf("lock %s: %w", lock.Name(), err)
	}
	info.PID, info.StateDir = os.Getpid(), dir
	st, err := store.Open(filepath.Join(dir, StoreName))
	if err != nil {
		return err
	}
	defer st.Close()
	d, err := openBackend(ctx, dir, &info, st)
	if err != nil {
		return fmt.Errorf("open the %s backend: %w", info.Backend, err)
	}

	if closer, ok := d.(interface{ Close() error }); ok {
		defer func() { err = errors.Join(err, closer.Close()) }()
	}

	sock := filepath.Join(dir, SocketName)
	// Holding the lock means any socket file here was left by a crashed daemon.
	if err := os.Remove(sock); err != nil && !os.IsNotExist(err) {
		return err
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	// Closing a unix listener removes its socket file. Close it here, before
	// the lock is released, even if Serve never got to track it.
	defer l.Close()
	if err := os.Chmod(sock, 0660); err != nil {
		return err
	}
	// Before serving, so no request sees the state the daemon died with;
	// clients connecting meanwhile wait in the listen backlog.
	if d != nil {
		if err := reconcile(ctx, st, d); err != nil {
			return fmt.Errorf("reconcile sandboxes with the %s backend: %w", info.Backend, err)
		}
	}

	// Shutdown ignores hijacked connections, so exec sessions would outlive
	// Run: every request runs under sessions, which is cancelled after
	// Shutdown, and Run waits for all handlers before it closes the store.
	sessions, endSessions := context.WithCancel(context.Background())
	defer endSessions()
	var mu sync.Mutex
	var active sync.WaitGroup
	requests := 0 // guarded by mu
	h := Handler(info, st, d)
	srv := &http.Server{
		BaseContext: func(net.Listener) context.Context { return sessions },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			if sessions.Err() != nil { // after a forced Close
				mu.Unlock()
				writeError(w, http.StatusServiceUnavailable, errors.New("the daemon is shutting down"))
				return
			}
			active.Add(1)
			requests++
			mu.Unlock()
			defer func() {
				mu.Lock()
				requests--
				mu.Unlock()
				active.Done()
			}()
			h.ServeHTTP(w, r)
		}),
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()
	slog.Info("daemon started", "socket", sock, "version", info.Version, "backend", info.Backend)
	if info.Warning != "" {
		slog.Warn(info.Warning, "backend", info.Backend)
	}

	select {
	case err = <-served:
		srv.Close()
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err = srv.Shutdown(shutdownCtx); err != nil {
			srv.Close() // drop connections that did not finish in time
		}
		<-served
	}
	mu.Lock()
	endSessions() // each exec relay closes both its WebSockets
	mu.Unlock()
	idle := make(chan struct{})
	go func() { active.Wait(); close(idle) }()
	select {
	case <-idle:
	case <-time.After(shutdownTimeout):
		mu.Lock()
		n := requests
		mu.Unlock()
		slog.Warn("requests still running after shutdown; waiting for cleanup", "requests", n)
		// A handler can still need the store for rollback after a slow
		// backend operation. A timeout must not release its store or lock.
		<-idle
	}
	slog.Info("daemon stopped")
	return err
}
