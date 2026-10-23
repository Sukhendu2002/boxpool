//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"

	"boxpool.run/boxpool/internal/agent"
	"boxpool.run/boxpool/internal/backend"
	"boxpool.run/boxpool/internal/client"
	"boxpool.run/boxpool/internal/daemon"
	"boxpool.run/boxpool/internal/store"
)

const prompt = "bp-test$ "

// TestMain runs main instead of the tests when a test re-executes this
// binary as the boxpool command, so the CLI gets a real controlling terminal.
func TestMain(m *testing.M) {
	if os.Getenv("BOXPOOL_TEST_MAIN") == "1" {
		// The "guest" is this machine; keep its profile out of the test.
		shell = []string{"/bin/sh", "-c", "PS1='" + prompt + "'; export PS1; exec /bin/sh -i"}
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeDriver has one agent, on this machine, for every sandbox.
type fakeDriver struct{ sock string }

func (d fakeDriver) Name() string { return "fake" }
func (d fakeDriver) Create(_ context.Context, id string, spec backend.Spec) (backend.Sandbox, error) {
	return backend.Sandbox{ID: id, Name: spec.Name}, nil
}
func (d fakeDriver) Start(context.Context, string) error             { return nil }
func (d fakeDriver) Stop(context.Context, string) error              { return nil }
func (d fakeDriver) Pause(context.Context, string) error             { return nil }
func (d fakeDriver) Resume(context.Context, string) error            { return nil }
func (d fakeDriver) Remove(context.Context, string) error            { return nil }
func (d fakeDriver) List(context.Context) ([]backend.Sandbox, error) { return nil, nil }
func (d fakeDriver) Dial(ctx context.Context, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", d.sock)
}

// connListener remembers its connections, so kill can drop them all like a
// dying daemon, including the hijacked WebSockets http.Server forgets.
type connListener struct {
	net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func (l *connListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.mu.Lock()
		l.conns = append(l.conns, c)
		l.mu.Unlock()
	}
	return c, err
}

func (l *connListener) kill() {
	l.Close()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.conns {
		c.Close()
	}
}

// beforeExec, if set, runs before the test daemon handles an exec request.
var beforeExec atomic.Pointer[func()]

// startDaemon serves the daemon API on dir's socket with a fake driver and
// a real agent, and returns a function that kills it.
func startDaemon(t *testing.T, dir string) (kill func()) {
	t.Helper()
	st, err := store.Open(filepath.Join(dir, daemon.StoreName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sock := filepath.Join(dir, "agent.sock")
	al, err := agent.Listen("unix:" + sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- agent.Serve(ctx, al, "test") }()
	t.Cleanup(func() { cancel(); <-served })

	l, err := net.Listen("unix", filepath.Join(dir, daemon.SocketName))
	if err != nil {
		t.Fatal(err)
	}
	cl := &connListener{Listener: l}
	info := daemon.Info{Backend: "fake", Warning: "test warning"}
	h := daemon.Handler(info, st, fakeDriver{sock})
	go http.Serve(cl, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f := beforeExec.Load(); f != nil && strings.HasSuffix(r.URL.Path, "/exec") {
			(*f)()
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(cl.kill)
	return cl.kill
}

// terminal is the controlling terminal of a boxpool command under test.
type terminal struct {
	t        *testing.T
	ptmx     *os.File
	mu       sync.Mutex
	out      bytes.Buffer
	consumed int
}

// expect waits for s in the output after the previous match.
func (term *terminal) expect(s string) {
	term.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		term.mu.Lock()
		i := strings.Index(term.out.String()[term.consumed:], s)
		if i >= 0 {
			term.consumed += i + len(s)
		}
		term.mu.Unlock()
		if i >= 0 {
			return
		}
	}
	term.mu.Lock()
	defer term.mu.Unlock()
	term.t.Fatalf("no %q in terminal output %q", s, term.out.String()[term.consumed:])
}

func (term *terminal) send(s string) {
	term.t.Helper()
	if _, err := term.ptmx.WriteString(s); err != nil {
		term.t.Fatal(err)
	}
}

// run is a boxpool command under test.
type run struct {
	cmd    *exec.Cmd
	tty    *os.File
	exit   string // file the command's exit code is written to
	stderr bytes.Buffer
	before *term.State // the terminal's settings before the command ran
}

// boxpool starts the boxpool command on a new terminal.
func boxpool(t *testing.T, dir string, args ...string) (*run, *terminal) {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ptmx.Close(); tty.Close() })
	if err := pty.Setsize(ptmx, &pty.Winsize{Rows: 24, Cols: 80}); err != nil {
		t.Fatal(err)
	}
	r := &run{tty: tty, exit: filepath.Join(t.TempDir(), "exit")}
	if r.before, err = term.GetState(int(tty.Fd())); err != nil {
		t.Fatal(err)
	}
	// The shell is the session leader and outlives boxpool, which runs in
	// its foreground process group: macOS revokes a terminal when its
	// session leader exits, and its settings could not be checked.
	script := `"$0" "$@"; echo $? >"$BP_EXIT"; exec sleep 600`
	r.cmd = exec.Command("/bin/sh", append([]string{"-c", script, os.Args[0]}, args...)...)
	r.cmd.Env = append(os.Environ(), "BOXPOOL_TEST_MAIN=1", "BOXPOOL_HOME="+dir, "TERM=xterm", "BP_EXIT="+r.exit)
	r.cmd.Stdin, r.cmd.Stdout, r.cmd.Stderr = tty, tty, &r.stderr
	r.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := r.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.cmd.Process.Kill(); r.cmd.Wait() })
	tm := &terminal{t: t, ptmx: ptmx}
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := ptmx.Read(b)
			tm.mu.Lock()
			tm.out.Write(b[:n])
			tm.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return r, tm
}

// wait waits for the command, checks its exit code and that the terminal's
// settings are as they were before, and returns its stderr.
func (r *run) wait(t *testing.T, want int) string {
	t.Helper()
	var code []byte
	for deadline := time.Now().Add(15 * time.Second); len(code) == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("boxpool did not exit")
		}
		code, _ = os.ReadFile(r.exit)
	}
	if got := strings.TrimSpace(string(code)); got != strconv.Itoa(want) {
		t.Errorf("exit code %s, want %d", got, want)
	}
	after, err := term.GetState(int(r.tty.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, r.before) {
		t.Errorf("terminal not restored: settings %+v, want %+v", after, r.before)
	}
	r.cmd.Process.Kill()
	r.cmd.Wait() // and stops writing stderr
	return r.stderr.String()
}

func TestSh(t *testing.T) {
	tmp, err := os.MkdirTemp("", "bp") // short: macOS caps socket paths at ~104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	kill := startDaemon(t, tmp)

	t.Run("exit", func(t *testing.T) {
		r, tm := boxpool(t, tmp, "sh", "--timing", "--name", "web")
		tm.expect(prompt)
		// The kernel signals SIGWINCH to the command, which tells the guest.
		if err := pty.Setsize(tm.ptmx, &pty.Winsize{Rows: 40, Cols: 120}); err != nil {
			t.Fatal(err)
		}
		for i := 0; ; i++ {
			tm.send("stty size\r")
			tm.expect(prompt)
			tm.mu.Lock()
			resized := strings.Contains(tm.out.String(), "40 120")
			tm.mu.Unlock()
			if resized {
				break
			}
			if i == 50 {
				t.Fatal("stty size in the guest never became 40 120")
			}
			time.Sleep(20 * time.Millisecond)
		}
		tm.send("exit 3\r")
		stderr := r.wait(t, 3)
		for _, want := range []string{"warning: test warning\n", "sandbox web (fake)\n", "time to prompt: ", "sandbox web is still running; reattach with: boxpool sh web\n"} {
			if !strings.Contains(stderr, want) {
				t.Errorf("stderr %q lacks %q", stderr, want)
			}
		}
	})

	t.Run("ctrl-d", func(t *testing.T) {
		r, tm := boxpool(t, tmp, "sh", "web")
		tm.expect(prompt)
		tm.send("\x04")
		r.wait(t, 0)
	})

	t.Run("resource flags", func(t *testing.T) {
		r, tm := boxpool(t, tmp, "sh", "--name", "limited", "--cpu", "1", "--mem", "512m", "--disk", "1g")
		tm.expect(prompt)
		tm.send("exit\r")
		r.wait(t, 0)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		sb, err := client.New(tmp).GetSandbox(ctx, "limited")
		if err != nil || sb.CPUs != 1 || sb.MemoryMB != 512 || sb.DiskGB != 1 {
			t.Fatalf("resource flags: %+v, %v", sb, err)
		}
	})

	t.Run("resize during handshake", func(t *testing.T) {
		reached, release := make(chan struct{}), make(chan struct{})
		hold := func() { close(reached); <-release }
		beforeExec.Store(&hold)
		defer beforeExec.Store(nil)
		r, tm := boxpool(t, tmp, "sh", "--name", "web")
		select {
		case <-reached:
		case <-time.After(10 * time.Second):
			t.Fatal("boxpool sh never sent the exec request")
		}
		if err := pty.Setsize(tm.ptmx, &pty.Winsize{Rows: 40, Cols: 120}); err != nil {
			t.Fatal(err)
		}
		close(release)
		tm.expect(prompt)
		tm.send("stty size\r")
		tm.expect("40 120")
		tm.send("exit\r")
		r.wait(t, 0)
	})

	t.Run("unknown name", func(t *testing.T) {
		r, _ := boxpool(t, tmp, "sh", "nope")
		if stderr := r.wait(t, 1); !strings.Contains(stderr, `sandbox "nope" not found`) {
			t.Errorf("stderr = %q", stderr)
		}
	})

	t.Run("daemon dies", func(t *testing.T) {
		r, tm := boxpool(t, tmp, "sh", "web")
		tm.expect(prompt)
		kill()
		if stderr := r.wait(t, 1); !strings.Contains(stderr, "connection to sandbox web lost") {
			t.Errorf("stderr = %q", stderr)
		}
	})
}

func TestShNeedsTerminal(t *testing.T) {
	cmd := exec.Command(os.Args[0], "sh")
	cmd.Env = append(os.Environ(), "BOXPOOL_TEST_MAIN=1")
	out, _ := cmd.CombinedOutput() // stdin is /dev/null
	if cmd.ProcessState.ExitCode() != 2 || !strings.Contains(string(out), "use boxpool exec") {
		t.Errorf("boxpool sh without a terminal = %d %q", cmd.ProcessState.ExitCode(), out)
	}
}

func TestParseSize(t *testing.T) {
	for _, c := range []struct {
		in   string
		unit int
		want int
		err  bool
	}{
		{"", 1, 0, false},
		{"512", 1, 512, false},
		{"512m", 1, 512, false},
		{"4G", 1, 4096, false},
		{"20g", 1024, 20, false},
		{"2048m", 1024, 2, false},
		{"20", 1024, 0, true}, // 20 MiB is not whole gigabytes
		{"0", 1, 0, true},
		{"-1g", 1, 0, true},
		{"1.5g", 1, 0, true},
		{"lots", 1, 0, true},
	} {
		got, err := parseSize(c.in, c.unit)
		if got != c.want || (err != nil) != c.err {
			t.Errorf("parseSize(%q, %d) = %d, %v", c.in, c.unit, got, err)
		}
	}
}
