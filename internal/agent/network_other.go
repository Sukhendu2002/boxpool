//go:build !linux

package agent

import "errors"

// PrepareGuestNetwork is only available on Linux, the guest operating system.
func PrepareGuestNetwork() error { return errors.New("guest network setup requires Linux") }
