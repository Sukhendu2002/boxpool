//go:build unix

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockAgent takes the exclusive lock that serialises boxpool up and down
// for a, or fails at once when another holds it; unlock releases it.
func lockAgent(a launchAgent) (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Dir(a.lock), 0750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(a.lock, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another boxpool up or down is running for %s; wait for it to finish, then try again", a.label)
		}
		return nil, fmt.Errorf("lock %s: %w", a.lock, err)
	}
	return func() { f.Close() }, nil
}

// alive reports whether process pid exists.
func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }
