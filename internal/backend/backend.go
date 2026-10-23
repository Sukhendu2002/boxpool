// Package backend defines the interface every sandbox backend implements.
//
// Planned backends: Firecracker (Linux with KVM), Docker (Linux fallback)
// and Apple's Virtualization framework (macOS on Apple silicon).
package backend

import (
	"context"
	"errors"
)

// ErrNotSupported is returned for operations a backend does not implement.
var ErrNotSupported = errors.New("not supported by this backend")

// Spec describes the sandbox to create.
type Spec struct {
	Name     string
	Image    string
	CPUs     int
	MemoryMB int
	DiskGB   int
}

// State is a sandbox's lifecycle state.
type State string

const (
	StateRunning State = "running"
	StatePaused  State = "paused"
	StateStopped State = "stopped"
)

// Sandbox is a sandbox as a backend reports it.
type Sandbox struct {
	ID    string
	Name  string
	State State
}

// Driver is implemented by each backend. The caller picks the ID, so the
// store and the backend name a sandbox the same way; it must be usable as a
// file name.
type Driver interface {
	Name() string
	Create(ctx context.Context, id string, spec Spec) (Sandbox, error)
	Start(ctx context.Context, id string) error
	Pause(ctx context.Context, id string) error
	Resume(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
	Remove(ctx context.Context, id string) error
	List(ctx context.Context) ([]Sandbox, error)
}
