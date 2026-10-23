//go:build linux || darwin

package daemon

var ShutdownTimeout = &shutdownTimeout

var OpenDriver = openDriver
