// Package backend defines the interface every sandbox backend implements.
//
// Planned backends: Firecracker (Linux with KVM), Docker (Linux fallback)
// and Apple's Virtualization framework (macOS on Apple silicon).
package backend

import (
	"context"
	"errors"
	"fmt"
)

// ErrNotSupported is returned for operations a backend does not implement.
var ErrNotSupported = errors.New("not supported by this backend")

// ErrUncleanStop is wrapped by Stop's error when the sandbox is stopped but
// the guest's shutdown did not complete, so its disk may be inconsistent and
// may need a filesystem check.
var ErrUncleanStop = errors.New("sandbox was not stopped cleanly")

// Spec describes the sandbox to create.
type Spec struct {
	Name     string
	Image    string
	CPUs     int
	MemoryMB int
	DiskGB   int
}

// WithDefaults fills unspecified limits. MemoryMB is in MiB and DiskGB in
// GiB. Negative limits are invalid; zero means the common backend default.
func (s Spec) WithDefaults() (Spec, error) {
	if s.CPUs < 0 || s.MemoryMB < 0 || s.DiskGB < 0 {
		return s, fmt.Errorf("CPU, memory and disk limits must not be negative")
	}
	if s.CPUs == 0 {
		s.CPUs = 2
	}
	if s.MemoryMB == 0 {
		s.MemoryMB = 2048
	}
	if s.DiskGB == 0 {
		s.DiskGB = 10
	}
	// Bound conversions to bytes and Docker's nanocpus without overflow.
	if int64(s.CPUs) > (1<<63-1)/1_000_000_000 || int64(s.MemoryMB) > (1<<63-1)/(1<<20) || int64(s.DiskGB) > (1<<63-1)/(1<<30) {
		return s, fmt.Errorf("CPU, memory or disk limit is too large")
	}
	return s, nil
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
