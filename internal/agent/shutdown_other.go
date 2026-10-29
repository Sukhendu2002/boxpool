//go:build !linux

package agent

// GuestShutdown is nil outside Linux: guests run Linux.
func GuestShutdown() *Shutdown { return nil }
