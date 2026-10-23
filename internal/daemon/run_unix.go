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
	"syscall"
	"time"

	"boxpool.run/boxpool/internal/store"
)

// shutdownTimeout is how long Run waits for in-flight requests on shutdown.
var shutdownTimeout = 5 * time.Second

// Run creates the state directory, takes the daemon lock, opens the sandbox
// store and the backend named by info.Backend (on Linux: firecracker or
// docker; any other name serves no backend) and serves the API on the
// socket in dir until ctx is cancelled. Only one daemon may run per state
// directory.
func Run(ctx context.Context, dir string, info Info) error {
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
	lock, err := os.OpenFile(filepath.Join(dir, "daemon.lock"), os.O_RDWR|os.O_CREATE, 0600)
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
	st, err := store.Open(filepath.Join(dir, StoreName))
	if err != nil {
		return err
	}
	defer st.Close()
	d, err := openDriver(ctx, dir, &info, st)
	if err != nil {
		return fmt.Errorf("open the %s backend: %w", info.Backend, err)
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

	srv := &http.Server{Handler: Handler(info, st, d)}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()
	slog.Info("daemon started", "socket", sock, "version", info.Version, "backend", info.Backend)
	if info.Warning != "" {
		slog.Warn(info.Warning, "backend", info.Backend)
	}

	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err = srv.Shutdown(shutdownCtx); err != nil {
		srv.Close() // drop connections that did not finish in time
	}
	<-served
	slog.Info("daemon stopped")
	return err
}
