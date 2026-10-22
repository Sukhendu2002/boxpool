//go:build linux || darwin

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"
)

// drainIdle is how long output may stay quiet after the process exits
// before the agent stops reading it: a background child can hold stdout
// open long after the command itself is gone.
const drainIdle = 250 * time.Millisecond

func runExec(ctx context.Context, c *websocket.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	typ, b, err := c.Read(ctx)
	if err != nil {
		return
	}
	var req ExecRequest
	if typ != websocket.MessageText || json.Unmarshal(b, &req) != nil {
		execError(ctx, c, errors.New("the first message must be a JSON exec request"))
		return
	}
	if len(req.Argv) == 0 {
		execError(ctx, c, errors.New("argv is empty"))
		return
	}

	cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	cmd.Env = append(os.Environ(), req.Env...)
	cmd.Dir = req.Cwd
	// The command leads its own process group (pipes) or session (PTY), so
	// killing the group takes its children with it.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	var (
		stdin io.WriteCloser
		outs  map[byte]*os.File
		tty   *os.File
	)
	if req.TTY {
		if req.Rows == 0 || req.Cols == 0 {
			req.Rows, req.Cols = 24, 80
		}
		tty, err = pty.StartWithSize(cmd, &pty.Winsize{Rows: req.Rows, Cols: req.Cols})
		stdin, outs = tty, map[byte]*os.File{Stdout: tty}
	} else {
		stdin, outs, err = startPipes(cmd)
	}
	if err != nil {
		execError(ctx, c, err)
		return
	}
	for _, f := range outs {
		defer f.Close()
	}

	// A lost client shows up as a failed read or write; either one cancels
	// ctx, which kills the process group.
	go func() {
		defer cancel()
		readInput(ctx, c, stdin, tty)
	}()
	exited := make(chan struct{})
	var pumps sync.WaitGroup
	for id, f := range outs {
		pumps.Add(1)
		go func() {
			defer pumps.Done()
			if pump(ctx, c, id, f, exited) != nil {
				cancel()
			}
		}()
	}
	cmd.Wait()
	close(exited)
	for _, f := range outs {
		f.SetReadDeadline(time.Now().Add(drainIdle))
	}
	pumps.Wait()
	if ctx.Err() != nil {
		return // the client is gone, nobody to tell
	}
	code := exitCode(cmd.ProcessState)
	finish(ctx, c, message{Type: "exit", Code: &code})
}

func startPipes(cmd *exec.Cmd) (io.WriteCloser, map[byte]*os.File, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	// Our own pipes rather than cmd.StdoutPipe: Wait closes those, which
	// would drop output still buffered in them.
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, nil, err
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	err = cmd.Start()
	outW.Close()
	errW.Close()
	if err != nil {
		outR.Close()
		errR.Close()
		return nil, nil, err
	}
	return stdin, map[byte]*os.File{Stdout: outR, Stderr: errR}, nil
}

// readInput applies the client's messages until the connection fails.
// shortcut: a process that stops reading stdin blocks this loop, so resize
// and disconnects are noticed only when it reads, writes output or exits;
// move stdin writes to their own goroutine if that bites.
func readInput(ctx context.Context, c *websocket.Conn, stdin io.WriteCloser, tty *os.File) {
	for {
		typ, b, err := c.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageBinary {
			if len(b) > 0 && b[0] == Stdin {
				stdin.Write(b[1:]) // fails only once the process stopped reading; drop
			}
			continue
		}
		var m message
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		switch {
		case m.Type == "resize" && tty != nil && m.Rows > 0 && m.Cols > 0:
			pty.Setsize(tty, &pty.Winsize{Rows: m.Rows, Cols: m.Cols})
		case m.Type == "stdin_eof" && tty == nil:
			stdin.Close()
		}
	}
}

// pump copies f to the client as stream id until f ends. Once the process
// has exited, it stops after drainIdle without output. A PTY needs no
// deadline: its reads fail as soon as the session leader exits.
func pump(ctx context.Context, c *websocket.Conn, id byte, f *os.File, exited <-chan struct{}) error {
	buf := make([]byte, maxMessage)
	buf[0] = id
	for {
		// Set before each read, so a slow client cannot use up the deadline.
		select {
		case <-exited:
			f.SetReadDeadline(time.Now().Add(drainIdle))
		default:
		}
		n, err := f.Read(buf[1:])
		if n > 0 {
			if err := c.Write(ctx, websocket.MessageBinary, buf[:1+n]); err != nil {
				return err
			}
		}
		if err != nil {
			return nil // EOF, EIO from a PTY that was hung up, or the drain deadline
		}
	}
}

// exitCode follows the shell convention: 128 plus the signal number for a
// process killed by a signal.
func exitCode(ps *os.ProcessState) int {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}
