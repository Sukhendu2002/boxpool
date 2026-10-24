package vz

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"time"

	"boxpool.run/boxpool/internal/backend"
)

type consoleCapture struct {
	input, inputWriter, outputReader, output, log *os.File
}

func newConsoleCapture(path string) (*consoleCapture, error) {
	c := &consoleCapture{}
	var err error
	c.log, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err == nil {
		c.input, c.inputWriter, err = os.Pipe()
	}
	if err == nil {
		c.outputReader, c.output, err = os.Pipe()
	}
	if err != nil {
		c.close()
		return nil, err
	}
	return c, nil
}

func (c *consoleCapture) close() {
	for _, f := range []*os.File{c.input, c.inputWriter, c.outputReader, c.output, c.log} {
		if f != nil {
			f.Close()
		}
	}
}

func (c *consoleCapture) run(stopped <-chan struct{}) {
	defer c.close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-stopped:
			// The framework can retain its copy of the pipe after the VM
			// stops. Allow trailing output to drain without waiting for EOF.
			c.outputReader.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		case <-done:
		}
	}()
	if err := copyConsole(c.log, c.outputReader); err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
		slog.Warn("capturing console log", "path", c.log.Name(), "err", err)
	}
}

// copyConsole rotates after every bounded read: a virtio console can emit
// megabytes between timer ticks. Keep draining if logging fails so a full
// host disk cannot block the guest on its console.
func copyConsole(out *os.File, in io.Reader) error {
	buf := make([]byte, 32<<10)
	var logErr error
	for {
		n, err := in.Read(buf)
		if n > 0 && logErr == nil {
			_, logErr = out.Write(buf[:n])
			if logErr == nil {
				logErr = backend.RotateConsole(out.Name())
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return logErr
			}
			return errors.Join(logErr, err)
		}
	}
}
