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
	"golang.org/x/sys/unix"
)

// drainIdle is how long output may stay quiet after the process exits
// before the agent stops reading it: a background child can hold stdout
// open long after the command itself is gone.
const drainIdle = 250 * time.Millisecond

// stdinQueue is how many stdin messages, of up to 32 KiB each, the agent
// holds for a command that has not read them yet: 1 MiB.
const stdinQueue = 32

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
		tty, err = startPTY(cmd, req.Rows, req.Cols)
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
	// Cancel kills the group only until Wait returns. A session that ends
	// without sending exit kills it again before runExec returns, which
	// takes background children left in it after the command exited.
	// shortcut: Wait has reaped the leader by then, so if every process in
	// the group has also gone and the kernel gives the id to a new group
	// leader before this kill, the kill hits that group; wait with WNOWAIT
	// and reap after the kill if that ever matters.
	pgid, exitSent := cmd.Process.Pid, false
	defer func() {
		if !exitSent {
			syscall.Kill(-pgid, syscall.SIGKILL)
		}
	}()

	// A lost client shows up as a failed read or write; either one cancels
	// ctx, which kills the process group.
	exited := make(chan struct{})
	in := make(chan []byte, stdinQueue)
	go writeStdin(in, stdin)
	go func() {
		defer cancel()
		readInput(ctx, c, in, tty, exited)
	}()
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
	exitSent = finish(ctx, c, message{Type: "exit", Code: &code})
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

// startPTY starts cmd in a new session on a new PTY and returns the PTY
// master.
func startPTY(cmd *exec.Cmd, rows, cols uint16) (*os.File, error) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		return nil, err
	}
	defer tty.Close()
	// pty.Open leaves the master in blocking mode, where a read cannot be
	// cut short by a deadline; a background child can keep the PTY open
	// after the session leader exits. Use a non-blocking duplicate, which
	// Go polls.
	fd, err := unix.FcntlInt(ptmx.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	ptmx.Close()
	if err != nil {
		return nil, err
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, err
	}
	master := os.NewFile(uintptr(fd), "ptmx")
	if rows == 0 || cols == 0 {
		rows, cols = 24, 80
	}
	if err := setSize(master, rows, cols); err != nil {
		master.Close()
		return nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		master.Close()
		return nil, err
	}
	return master, nil
}

// setSize sets the window size of the PTY whose master is f. pty.Setsize
// would put f back in blocking mode.
func setSize(f *os.File, rows, cols uint16) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	if err := rc.Control(func(fd uintptr) {
		err = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
	}); err != nil {
		return err
	}
	return err
}

// readInput applies the client's messages until the connection fails.
// Stdin data goes to writeStdin through in, so a command that does not read
// its stdin does not stop this loop from seeing resizes and a lost client.
// shortcut: once in is full (1 MiB on top of the pipe or PTY buffer) this
// loop waits for room, so a client that disconnects then is noticed only
// when the command reads stdin, writes output or exits, or the agent stops;
// read the connection in its own goroutine if a client hits that.
func readInput(ctx context.Context, c *websocket.Conn, in chan<- []byte, tty *os.File, exited <-chan struct{}) {
	defer close(in)
	// send queues b, or drops it once the command has exited.
	send := func(b []byte) bool {
		select {
		case in <- b:
		case <-exited:
		case <-ctx.Done():
			return false
		}
		return true
	}
	for {
		typ, b, err := c.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageBinary {
			if len(b) > 1 && b[0] == Stdin && !send(b[1:]) {
				return
			}
			continue
		}
		var m message
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		switch {
		case m.Type == "resize" && tty != nil && m.Rows > 0 && m.Cols > 0:
			setSize(tty, m.Rows, m.Cols)
		case m.Type == "stdin_eof" && tty == nil:
			if !send(nil) {
				return
			}
		}
	}
}

// writeStdin writes each chunk from in to stdin; nil closes stdin. A write
// blocked on a command that does not read ends when Wait closes the stdin
// pipe or runExec closes the PTY.
func writeStdin(in <-chan []byte, stdin io.WriteCloser) {
	for b := range in {
		if b == nil {
			stdin.Close()
		} else {
			stdin.Write(b) // fails only once the process stopped reading; drop
		}
	}
}

// pump copies f to the client as stream id until f ends. Once the process
// has exited, it stops after drainIdle without output.
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
			return nil // EOF, EIO from a PTY whose other side closed, or the drain deadline
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
