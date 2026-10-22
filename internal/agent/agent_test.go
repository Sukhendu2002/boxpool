//go:build linux || darwin

package agent

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
