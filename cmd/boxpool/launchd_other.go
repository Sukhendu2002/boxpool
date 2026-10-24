//go:build !unix

package main

import "errors"

// lockAgent fails: launchd runs only on macOS.
func lockAgent(launchAgent) (func(), error) { return nil, errors.ErrUnsupported }

func alive(int) bool { return false }
