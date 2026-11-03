package agent

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// PrepareGuestNetwork brings up loopback before a VM agent serves requests.
// A VM without a NIC has no kernel IP autoconfiguration to do this for it.
// Docker agents must not call this: the Engine configures their network, and
// they intentionally lack CAP_NET_ADMIN. No external interface is changed.
func PrepareGuestNetwork() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("loopback socket: %w", err)
	}
	defer unix.Close(fd)
	req, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, req); err != nil {
		return fmt.Errorf("read loopback flags: %w", err)
	}
	if req.Uint16()&unix.IFF_UP != 0 {
		return nil
	}
	req.SetUint16(req.Uint16() | unix.IFF_UP)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, req); err != nil {
		return fmt.Errorf("bring up loopback: %w", err)
	}
	return nil
}
