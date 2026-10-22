package agent

import (
	"errors"
	"fmt"
	"os"
)

func listeningPorts() ([]Port, error) {
	ports := []Port{}
	for _, name := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(name)
		if errors.Is(err, os.ErrNotExist) && name == "/proc/net/tcp6" {
			continue // IPv6 may be disabled in the guest.
		}
		if err != nil {
			return nil, err
		}
		p, err := parsePorts(f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		ports = append(ports, p...)
	}
	return ports, nil
}
