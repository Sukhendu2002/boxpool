//go:build !unix

package main

import "os"

// notifyResize does nothing: there is no SIGWINCH.
func notifyResize(c chan<- os.Signal) {}
