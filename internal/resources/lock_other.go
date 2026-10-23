//go:build !linux && !darwin

package resources

import (
	"context"
	"fmt"
)

func (m *Manager) locked(context.Context, func() error) error {
	return fmt.Errorf("resource reservations require Unix file locking")
}

func (m *Manager) LockSandbox(context.Context, string, string) (func(), error) {
	return nil, fmt.Errorf("resource reservations require Unix file locking")
}
