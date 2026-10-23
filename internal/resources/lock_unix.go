//go:build linux || darwin

package resources

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func (m *Manager) locked(ctx context.Context, fn func() error) error {
	unlock, err := lockFile(ctx, filepath.Join(m.StateDir, "resources.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

// LockSandbox serializes lifecycle changes for one ID. Otherwise a Remove
// could erase the reservation while its Create is still allocating Engine
// resources. The caller validates kind/id and holds the lock through cleanup.
func (m *Manager) LockSandbox(ctx context.Context, kind, id string) (func(), error) {
	return lockFile(ctx, filepath.Join(m.StateDir, "resource-locks", kind+"-"+id+".lock"))
}

func lockFile(ctx context.Context, path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	failed := true
	defer func() {
		if failed {
			f.Close()
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	failed = false
	return func() { f.Close() }, nil // close releases flock; never unlink a live lock file
}
