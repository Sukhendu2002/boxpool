//go:build linux || darwin

package agent

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// serve runs an agent on a unix socket in a temp dir and returns a client
// for it. It avoids t.TempDir because macOS limits unix socket paths to
// about 104 bytes.
func serve(t *testing.T) *Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "bpa")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "run", "agent.sock")
	l, err := Listen("unix:" + sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, l, "test") }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve() = %v", err)
		}
	})
	return NewClient(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	})
}

// output is an io.Writer that tests can wait on.
type output struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(p)
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

func (o *output) waitFor(t *testing.T, s string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(o.String(), s); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("output %q does not contain %q after 10s", o.String(), s)
		}
	}
}

func testContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// run executes req with stdin and returns its output and exit code.
func run(t *testing.T, c *Client, req ExecRequest, stdin string) (stdout, stderr string, code int, err error) {
	t.Helper()
	var out, errOut output
	s, err := c.Exec(testContext(t), req, &out, &errOut)
	if err != nil {
		t.Fatal(err)
	}
	if stdin != "" {
		if _, err := s.Write([]byte(stdin)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CloseStdin(); err != nil {
		t.Fatal(err)
	}
	code, err = s.Wait()
	return out.String(), errOut.String(), code, err
}

func TestVersion(t *testing.T) {
	v, err := serve(t).Version(context.Background())
	if err != nil || v != (Version{Protocol: ProtocolVersion, Agent: "test"}) {
		t.Errorf("Version() = %+v, %v", v, err)
	}
}

func TestExec(t *testing.T) {
	c := serve(t)
	cwd, err := filepath.EvalSymlinks(t.TempDir()) // /tmp is a symlink on macOS
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                 string
		req                  ExecRequest
		stdin                string
		stdout, stderr, code string
	}{
		{"exit code", ExecRequest{Argv: []string{"sh", "-c", "echo out; echo err >&2; exit 3"}}, "", "out\n", "err\n", "3"},
		{"env", ExecRequest{Argv: []string{"sh", "-c", "echo $FOO"}, Env: []string{"FOO=bar"}}, "", "bar\n", "", "0"},
		{"cwd", ExecRequest{Argv: []string{"pwd"}, Cwd: cwd}, "", cwd + "\n", "", "0"},
		{"stdin", ExecRequest{Argv: []string{"cat"}}, "hello", "hello", "", "0"},
		{"signal", ExecRequest{Argv: []string{"sh", "-c", "kill -TERM $$"}}, "", "", "", "143"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code, err := run(t, c, tc.req, tc.stdin)
			if err != nil || stdout != tc.stdout || stderr != tc.stderr || strconv.Itoa(code) != tc.code {
				t.Errorf("got stdout %q, stderr %q, code %d, err %v; want %q, %q, %s", stdout, stderr, code, err, tc.stdout, tc.stderr, tc.code)
			}
		})
	}
}

func TestExecErrors(t *testing.T) {
	c := serve(t)
	for _, tc := range []struct {
		name string
		req  ExecRequest
		want string
	}{
		{"not found", ExecRequest{Argv: []string{"boxpool-no-such-command"}}, "executable file not found"},
		{"empty argv", ExecRequest{}, "argv is empty"},
		{"bad cwd", ExecRequest{Argv: []string{"pwd"}, Cwd: "/boxpool/no/such/dir"}, "no such file or directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, err := run(t, c, tc.req, ""); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestExecDrainsLargeOutput(t *testing.T) {
	const n = 1 << 20 // many messages, so chunking and draining both matter
	stdout, _, code, err := run(t, serve(t), ExecRequest{Argv: []string{"sh", "-c", "head -c " + strconv.Itoa(n) + " /dev/zero"}}, "")
	if err != nil || len(stdout) != n || code != 0 {
		t.Errorf("got %d bytes, code %d, err %v; want %d bytes", len(stdout), code, err, n)
	}
}

func TestExecDoesNotWaitForBackgroundChild(t *testing.T) {
	c := serve(t)
	for _, tty := range []bool{false, true} {
		t.Run("tty="+strconv.FormatBool(tty), func(t *testing.T) {
			start := time.Now()
			// The sleep keeps stdout open after sh exits; it ignores the
			// SIGHUP a PTY session leader's exit sends.
			stdout, _, code, err := run(t, c, ExecRequest{Argv: []string{"sh", "-c", "echo hi; (trap '' HUP; sleep 3) &"}, TTY: tty}, "")
			if err != nil || strings.TrimRight(stdout, "\r\n") != "hi" || code != 0 {
				t.Errorf("got stdout %q, code %d, err %v", stdout, code, err)
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Errorf("exec took %v: waited for the background child", d)
			}
		})
	}
}

func TestDisconnectKillsProcessGroup(t *testing.T) {
	for _, tty := range []bool{false, true} {
		t.Run("tty="+strconv.FormatBool(tty), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var out output
			s, err := serve(t).Exec(ctx, ExecRequest{Argv: []string{"sh", "-c", "sleep 100 & echo pid=$!; wait"}, TTY: tty}, &out, &out)
			if err != nil {
				t.Fatal(err)
			}
			out.waitFor(t, "\n")
			pid, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(out.String(), "pid=")))
			if err != nil {
				t.Fatalf("output %q: %v", out.String(), err)
			}
			cancel()
			if _, err := s.Wait(); err == nil {
				t.Error("Wait() = nil after the session was cancelled")
			}
			for deadline := time.Now().Add(5 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(10 * time.Millisecond) {
				if time.Now().After(deadline) {
					syscall.Kill(pid, syscall.SIGKILL)
					t.Fatalf("background child %d still running after disconnect", pid)
				}
			}
		})
	}
}

// execPTY starts argv on a PTY and returns the session and its output.
func execPTY(t *testing.T, argv ...string) (*Session, *output) {
	t.Helper()
	var out output
	s, err := serve(t).Exec(testContext(t), ExecRequest{Argv: argv, Env: []string{"TERM=xterm"}, TTY: true, Rows: 24, Cols: 80}, &out, &out)
	if err != nil {
		t.Fatal(err)
	}
	return s, &out
}

func wait(t *testing.T, s *Session, out *output, want int) {
	t.Helper()
	if code, err := s.Wait(); err != nil || code != want {
		t.Errorf("Wait() = %d, %v; want %d (output %q)", code, err, want, out.String())
	}
}

func TestPTYResize(t *testing.T) {
	s, out := execPTY(t, "sh", "-c", "stty size; read x; stty size")
	out.waitFor(t, "24 80")
	if err := s.Resize(40, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	wait(t, s, out, 0)
	out.waitFor(t, "40 100")
}

func TestPTYCtrlC(t *testing.T) {
	s, out := execPTY(t, "sh", "-c", "echo ready; exec sleep 100")
	out.waitFor(t, "ready")
	if _, err := s.Write([]byte{0x03}); err != nil {
		t.Fatal(err)
	}
	wait(t, s, out, 130)
}

func TestPTYVim(t *testing.T) {
	vim, err := exec.LookPath("vim")
	if err != nil {
		if vim, err = exec.LookPath("vi"); err != nil {
			t.Skip("no vim or vi on this machine")
		}
	}
	s, out := execPTY(t, vim)
	out.waitFor(t, "\x1b") // vim has drawn its screen
	if _, err := s.Write([]byte("\x1b:q!\r")); err != nil {
		t.Fatal(err)
	}
	wait(t, s, out, 0)
}

func TestServeStopKillsSessions(t *testing.T) {
	dir, err := os.MkdirTemp("", "bpa")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "agent.sock")
	l, err := Listen("unix:" + sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, l, "test") }()
	c := NewClient(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	})
	var out output
	s, err := c.Exec(testContext(t), ExecRequest{Argv: []string{"sh", "-c", "echo pid=$$; exec sleep 100"}}, &out, &out)
	if err != nil {
		t.Fatal(err)
	}
	out.waitFor(t, "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(out.String(), "pid=")))
	if err != nil {
		t.Fatalf("output %q: %v", out.String(), err)
	}
	stop()
	if err := <-done; err != nil {
		t.Errorf("Serve() = %v", err)
	}
	// Serve has returned, so the process must already be dead and reaped.
	if err := syscall.Kill(pid, 0); err == nil {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("process %d still running after Serve returned", pid)
	}
	if _, err := s.Wait(); err == nil {
		t.Error("Wait() = nil after the agent stopped")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("socket still present after Serve returned: %v", err)
	}
}

func TestExecLargeStdin(t *testing.T) {
	in := strings.Repeat("0123456789abcdef", 1<<18) // 4 MiB: more than the agent buffers
	stdout, _, code, err := run(t, serve(t), ExecRequest{Argv: []string{"cat"}}, in)
	if err != nil || stdout != in || code != 0 {
		t.Errorf("got %d bytes (intact: %v), code %d, err %v; want %d bytes", len(stdout), stdout == in, code, err, len(in))
	}
}

var pidRE = regexp.MustCompile(`pid=(\d+)\r?\n`)

// waitPid waits for a pid=<n> line in out and returns n.
func waitPid(t *testing.T, out *output) int {
	t.Helper()
	out.waitFor(t, "pid=")
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if m := pidRE.FindStringSubmatch(out.String()); m != nil {
			pid, _ := strconv.Atoi(m[1])
			t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pid line in output %q", out.String())
		}
	}
}

// waitGone waits for process pid to be gone.
func waitGone(t *testing.T, pid int, what string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s %d still running 5s after the session was aborted", what, pid)
		}
	}
}

func TestDisconnectWithUnreadStdin(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out output
	s, err := serve(t).Exec(ctx, ExecRequest{Argv: []string{"sh", "-c", "echo pid=$$; exec sleep 30"}}, &out, &out)
	if err != nil {
		t.Fatal(err)
	}
	pid := waitPid(t, &out)
	if _, err := s.Write(make([]byte, 1<<20)); err != nil {
		t.Fatalf("Write() = %v: the agent stopped reading", err)
	}
	cancel()
	waitGone(t, pid, "command")
}

// Pipes only: a PTY is hung up when its session leader exits, so a child
// cannot keep the drain going there. The kill on abort is the same code.
func TestAbortKillsBackgroundChildAfterExit(t *testing.T) {
	// sh exits at once; the child's output keeps the session in its drain
	// until the client disconnects. It ignores SIGPIPE, so only a kill ends
	// it once the agent closes its stdout.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out output
	s, err := serve(t).Exec(ctx, ExecRequest{Argv: []string{"sh", "-c", "trap '' PIPE; (while :; do echo; sleep 0.1; done) & echo pid=$!"}}, &out, &out)
	if err != nil {
		t.Fatal(err)
	}
	pid := waitPid(t, &out)
	time.Sleep(300 * time.Millisecond) // sh has exited and been reaped
	cancel()
	if _, err := s.Wait(); err == nil {
		t.Error("Wait() = nil after the session was cancelled")
	}
	waitGone(t, pid, "background child")
}

func TestExitLeavesBackgroundChild(t *testing.T) {
	c := serve(t)
	for _, tty := range []bool{false, true} {
		t.Run("tty="+strconv.FormatBool(tty), func(t *testing.T) {
			var out output
			s, err := c.Exec(testContext(t), ExecRequest{Argv: []string{"sh", "-c", "trap '' HUP; sleep 30 & echo pid=$!"}, TTY: tty}, &out, &out)
			if err != nil {
				t.Fatal(err)
			}
			pid := waitPid(t, &out)
			wait(t, s, &out, 0)
			if err := syscall.Kill(pid, 0); err != nil {
				t.Errorf("background child %d gone after a normal exit: %v", pid, err)
			}
		})
	}
}

// tempSock returns a unix socket path in a new temp dir; see serve.
func tempSock(t *testing.T) string {
	dir, err := os.MkdirTemp("", "bpa")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "agent.sock")
}

func TestListenRefusesLiveSocket(t *testing.T) {
	sock := tempSock(t)
	l, err := Listen("unix:" + sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if l2, err := Listen("unix:" + sock); err == nil || !strings.Contains(err.Error(), "another agent is listening") {
		t.Errorf("second Listen() = %v, want another agent is listening", err)
		if l2 != nil {
			l2.Close()
		}
	}
	// The first listener still owns the path.
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	a, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
}

func TestListenReplacesStaleSocket(t *testing.T) {
	sock := tempSock(t)
	old, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	old.(*net.UnixListener).SetUnlinkOnClose(false) // like an agent that was killed
	old.Close()
	l, err := Listen("unix:" + sock)
	if err != nil {
		t.Fatalf("Listen() over a stale socket = %v", err)
	}
	l.Close()
}

func TestListenRefusesNonSocket(t *testing.T) {
	sock := tempSock(t)
	if err := os.WriteFile(sock, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	if l, err := Listen("unix:" + sock); err == nil {
		l.Close()
		t.Fatal("Listen() over a regular file succeeded")
	}
	if b, err := os.ReadFile(sock); err != nil || string(b) != "keep" {
		t.Errorf("file changed: %q, %v", b, err)
	}
}
