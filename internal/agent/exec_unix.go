//go:build linux || darwin

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// maxStdin is how many bytes of stdin the agent holds for a command that
// has not read them yet.
const maxStdin = 1 << 20

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
		slave *os.File
	)
	if req.TTY {
		tty, slave, err = startPTY(cmd, req.Rows, req.Cols)
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
	// shortcut: the abort kill can hit a reused process group id after the
	// leader is reaped; acceptable inside a single-tenant sandbox, upgrade
	// to waitid(WNOWAIT) on Linux before the agent manages long-lived
	// services.
	pgid, exitSent := cmd.Process.Pid, false
	defer func() {
		if !exitSent {
			syscall.Kill(-pgid, syscall.SIGKILL)
		}
	}()

	// A lost client shows up as a failed read or write; either one cancels
	// ctx, which kills the process group.
	exited := make(chan struct{})
	in := &stdinQueue{ready: make(chan struct{}, 1), room: make(chan struct{}, 1)}
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
	// On macOS a process exiting on a PTY waits until its output has been
	// read. Once the client is gone nothing reads the master, so an abort
	// kills the group, then closes the master, before Wait can return.
	stopClose := func() bool { return true }
	if tty != nil {
		stopClose = context.AfterFunc(ctx, func() {
			syscall.Kill(-pgid, syscall.SIGKILL)
			tty.Close()
		})
	}
	cmd.Wait()
	// Held until now: on macOS, output still unread when the last slave
	// descriptor closes is lost, as with a command that exits at once.
	// Before stopClose, so an abort still unblocks a close that drains.
	if slave != nil {
		slave.Close()
	}
	stopClose()
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
// master and slave.
func startPTY(cmd *exec.Cmd, rows, cols uint16) (_, _ *os.File, err error) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil {
			tty.Close()
		}
	}()
	// pty.Open leaves the master in blocking mode, where a read cannot be
	// cut short by a deadline; a background child can keep the PTY open
	// after the session leader exits. Use a non-blocking duplicate, which
	// Go polls.
	fd, err := unix.FcntlInt(ptmx.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	ptmx.Close()
	if err != nil {
		return nil, nil, err
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, nil, err
	}
	master := os.NewFile(uintptr(fd), "ptmx")
	if rows == 0 || cols == 0 {
		rows, cols = 24, 80
	}
	if err := setSize(master, rows, cols); err != nil {
		master.Close()
		return nil, nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		master.Close()
		return nil, nil, err
	}
	return master, tty, nil
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
// shortcut: a client that queues more than 1 MiB of stdin to a command
// that never reads it, then disconnects, is noticed only when the command
// reads, writes or exits, or the agent stops; protocol-level flow control
// is planned with the daemon client in #15.
func readInput(ctx context.Context, c *websocket.Conn, in *stdinQueue, tty *os.File, exited <-chan struct{}) {
	defer in.add(nil, false, true)
	// send queues b, or drops it once the command has exited.
	send := func(b []byte, eof bool) bool {
		for !in.add(b, eof, false) {
			select {
			case <-in.room:
			case <-exited:
				return true
			case <-ctx.Done():
				return false
			}
		}
		return true
	}
	for {
		typ, b, err := c.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageBinary {
			if len(b) > 1 && b[0] == Stdin && !send(b[1:], false) {
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
			if !send(nil, true) {
				return
			}
		}
	}
}

// stdinQueue holds stdin the command has not read yet, up to maxStdin
// bytes. readInput adds to it and writeStdin writes it out.
type stdinQueue struct {
	mu        sync.Mutex
	data      []byte
	held      int           // len(data) plus the bytes writeStdin is writing
	eof, done bool          // close stdin after data; readInput has returned
	ready     chan struct{} // 1 slot: data, eof or done was added
	room      chan struct{} // 1 slot: held went down
}

// add queues b, then eof and done if set. It reports false, queueing
// nothing, when b does not fit. Data after eof is dropped.
func (q *stdinQueue) add(b []byte, eof, done bool) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.eof {
		if q.held+len(b) > maxStdin {
			return false
		}
		q.data = append(q.data, b...)
		q.held += len(b)
	}
	q.eof, q.done = q.eof || eof, q.done || done
	notify(q.ready)
	return true
}

func notify(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// writeStdin writes q to stdin and closes stdin at eof. A write blocked on
// a command that does not read ends when Wait closes the stdin pipe or
// runExec closes the PTY.
func writeStdin(q *stdinQueue, stdin io.WriteCloser) {
	for {
		q.mu.Lock()
		b, eof, done := q.data, q.eof, q.done
		q.data = nil
		q.mu.Unlock()
		switch {
		case len(b) > 0:
			stdin.Write(b) // fails only once the process stopped reading; drop
			q.mu.Lock()
			q.held -= len(b)
			q.mu.Unlock()
			notify(q.room)
		case eof:
			stdin.Close()
			return
		case done:
			return
		default:
			<-q.ready
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

// lockPath takes an exclusive flock on the file at path, creating it, and
// returns it; closing it releases the lock. It returns nil, nil when another
// process holds the lock.
func lockPath(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, nil
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return f, nil
}

// exitCode follows the shell convention: 128 plus the signal number for a
// process killed by a signal.
func exitCode(ps *os.ProcessState) int {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}
