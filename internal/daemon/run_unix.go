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
)

// Run creates the state directory, takes the daemon lock and serves the API
// on the socket in dir until ctx is cancelled. Only one daemon may run per
// state directory.
func Run(ctx context.Context, dir string, info Info) error {
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
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

	sock := filepath.Join(dir, SocketName)
	// Holding the lock means any socket file here was left by a crashed daemon.
	if err := os.Remove(sock); err != nil && !os.IsNotExist(err) {
		return err
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	if err := os.Chmod(sock, 0660); err != nil {
		l.Close()
		return err
	}

	srv := &http.Server{Handler: Handler(info)}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()
	slog.Info("daemon started", "socket", sock, "version", info.Version, "backend", info.Backend)

	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Shutdown closes the listener, and closing a unix listener removes its socket file.
	err = srv.Shutdown(shutdownCtx)
	slog.Info("daemon stopped")
	return err
}
