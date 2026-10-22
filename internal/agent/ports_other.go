//go:build !linux

package agent

import "errors"

func listeningPorts() ([]Port, error) {
	return nil, errors.New("listening ports unsupported: requires Linux /proc/net/tcp")
}
