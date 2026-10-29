//go:build !darwin || !cgo

package power

import (
	"errors"
	"runtime"
)

func PreventSleep() (func() error, error) {
	if runtime.GOOS == "darwin" {
		return nil, errors.New("idle-sleep prevention requires a build with cgo")
	}
	return func() error { return nil }, nil
}

func BatteryWarning() string {
	if runtime.GOOS == "darwin" {
		return "power source checks require a build with cgo"
	}
	return ""
}
