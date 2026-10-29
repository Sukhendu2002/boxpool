package docker

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/agent"
)

// countingListener counts the connections it accepts.
type countingListener struct {
	net.Listener
	n atomic.Int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.n.Add(1)
	}
	return c, err
}

// serveAgent serves a real agent on a new socket at path.
func serveAgent(t *testing.T, path, version string) *countingListener {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	cl := &countingListener{Listener: l}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		agent.Serve(ctx, cl, version, nil)
	}()
	t.Cleanup(func() { cancel(); <-done })
	return cl
}

// runEcho runs echo through c and returns its output.
func runEcho(ctx context.Context, c *agent.Client) (string, error) {
	var out bytes.Buffer
	s, err := c.Exec(ctx, agent.ExecRequest{Argv: []string{"echo", "ran"}}, &out, &out)
	if err != nil {
		return "", err
	}
	if _, err := s.Wait(); err != nil {
		return "", err
	}
	return out.String(), nil
}

// A guest replaces its agent socket, in the directory it shares with the
// host, with a symlink to another sandbox's socket. Dialled as a path, the
// symlink is followed with the daemon's permissions, so the attacker's
// sandbox reaches the victim's agent: Dial must refuse it.
func TestDialRefusesSymlink(t *testing.T) {
	ctx := context.Background()
	d := &Driver{root: shortTemp(t)}
	attacker, victim := sandboxLayout(d.root, "a"), sandboxLayout(d.root, "v")
	vl := serveAgent(t, victim.sock, "victim")
	if err := os.MkdirAll(attacker.agent, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim.sock, attacker.sock); err != nil {
		t.Fatal(err)
	}

	// The old Dial: a plain dial of the path reaches the victim.
	old := agent.NewClient(func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", attacker.sock)
	})
	defer old.CloseIdleConnections()
	v, err := old.Version(ctx)
	out, xerr := runEcho(ctx, old)
	if err != nil || v.Agent != "victim" || xerr != nil || out != "ran\n" {
		t.Fatalf("control: plain dial through the symlink: %+v, %v; exec %q, %v", v, err, out, xerr)
	}
	t.Logf("control: plain dial through the symlink reached the %s agent and ran echo", v.Agent)

	vl.n.Store(0)
	c := agent.NewClient(func(ctx context.Context) (net.Conn, error) { return d.Dial(ctx, "a") })
	defer c.CloseIdleConnections()
	if v, err := c.Version(ctx); err == nil {
		t.Errorf("Version through Dial reached %+v", v)
	} else if !strings.Contains(err.Error(), "not a unix socket") {
		t.Errorf("Version through Dial: %v, want a refusal", err)
	}
	if _, err := runEcho(ctx, c); err == nil {
		t.Error("Exec through Dial succeeded")
	}
	if n := vl.n.Load(); n != 0 {
		t.Errorf("the victim's agent got %d connections", n)
	}
}

// Dial refuses whatever is not a socket, without blocking on a FIFO.
func TestDialRefusesNonSockets(t *testing.T) {
	ctx := context.Background()
	dir := shortTemp(t)
	for name, mk := range map[string]func(string) error{
		"file":    func(p string) error { return os.WriteFile(p, nil, 0666) },
		"dir":     func(p string) error { return os.Mkdir(p, 0777) },
		"fifo":    func(p string) error { return syscall.Mkfifo(p, 0666) },
		"dangle":  func(p string) error { return os.Symlink(filepath.Join(dir, "none"), p) },
		"missing": func(string) error { return nil },
	} {
		p := filepath.Join(dir, name)
		if err := mk(p); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		c, err := dialSocket(ctx, p)
		cancel()
		if err == nil {
			c.Close()
			t.Errorf("%s: dialled", name)
		} else {
			t.Logf("%s: %v", name, err)
		}
	}
}

// Once Dial has resolved the path, swapping it for a symlink to another
// sandbox's socket does not redirect the connection: it goes to the inode
// first found.
func TestDialPinsSocket(t *testing.T) {
	ctx := context.Background()
	dir := shortTemp(t)
	own, victim := filepath.Join(dir, "own.sock"), filepath.Join(dir, "victim.sock")
	ol, vl := serveAgent(t, own, "own"), serveAgent(t, victim, "victim")

	fd, err := openSocket(own)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	// The swap lands between resolution and connect.
	tmp := filepath.Join(dir, "tmp")
	if err := os.Symlink(victim, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, own); err != nil {
		t.Fatal(err)
	}
	conn, err := dialFD(ctx, fd, own)
	if err != nil {
		t.Fatal(err)
	}
	c := agent.NewClient(func(context.Context) (net.Conn, error) { return conn, nil })
	defer c.CloseIdleConnections()
	if v, err := c.Version(ctx); err != nil || v.Agent != "own" {
		t.Errorf("Version after the swap: %+v, %v; want the own agent", v, err)
	}
	if ol.n.Load() != 1 || vl.n.Load() != 0 {
		t.Errorf("connections: own %d, victim %d; want 1 and 0", ol.n.Load(), vl.n.Load())
	}
}

// A guest flipping its socket path between its own socket and a symlink to
// the victim's, as fast as it can, never gets a dial to the victim.
func TestDialRace(t *testing.T) {
	ctx := context.Background()
	d := &Driver{root: shortTemp(t)}
	l, victim := sandboxLayout(d.root, "a"), sandboxLayout(d.root, "v")
	ol, vl := serveAgent(t, filepath.Join(l.agent, "real.sock"), "own"), serveAgent(t, victim.sock, "victim")
	link, hard := filepath.Join(l.agent, "link"), filepath.Join(l.agent, "hard")

	stop := make(chan struct{})
	flipped := make(chan int)
	go func() {
		n := 0
		defer func() { flipped <- n }()
		for ; ; n++ {
			select {
			case <-stop:
				return
			default:
			}
			// Atomic replacements: a hard link to the own socket, then a
			// symlink to the victim's.
			os.Link(filepath.Join(l.agent, "real.sock"), hard)
			os.Rename(hard, l.sock)
			os.Symlink(victim.sock, link)
			os.Rename(link, l.sock)
		}
	}()
	var ok, refused int
	deadline := time.Now().Add(2 * time.Second)
	for i := 0; i < 5000 && time.Now().Before(deadline); i++ {
		c, err := d.Dial(ctx, "a")
		if err != nil {
			refused++
			continue
		}
		ok++
		c.Close()
	}
	close(stop)
	t.Logf("%d dials connected, %d refused, %d flips", ok, refused, <-flipped)
	if n := vl.n.Load(); n != 0 {
		t.Errorf("the victim's agent got %d connections", n)
	}
	if ok == 0 || refused == 0 {
		t.Error("the race never hit one of the two states")
	}
	// Accepts can trail the dials that made them.
	for i := 0; i < 100 && ol.n.Load() < int64(ok); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if ol.n.Load() != int64(ok) {
		t.Errorf("own agent accepted %d of %d connections", ol.n.Load(), ok)
	}
}

// Dial leaves no descriptor behind, connected or refused, and is not bound
// by the 108 bytes of a socket address.
func TestDialDescriptors(t *testing.T) {
	ctx := context.Background()
	dir := shortTemp(t)
	sock := filepath.Join(dir, "s.sock")
	serveAgent(t, sock, "own")
	long := filepath.Join(dir, strings.Repeat("d", 100), strings.Repeat("e", 100))
	if err := os.MkdirAll(long, 0700); err != nil {
		t.Fatal(err)
	}
	long = filepath.Join(long, "agent.sock")
	if err := os.Rename(sock, long); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sock, nil, 0666); err != nil {
		t.Fatal(err)
	}
	fds := func() int {
		e, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		return len(e)
	}
	dial := func() {
		c, err := dialSocket(ctx, long)
		if err != nil {
			t.Fatalf("dial a %d-byte path: %v", len(long), err)
		}
		c.Close()
		if _, err := dialSocket(ctx, sock); err == nil {
			t.Fatal("dialled a file")
		}
	}
	dial() // warm up the runtime's own descriptors
	before := fds()
	for i := 0; i < 200; i++ {
		dial()
	}
	// The agent, in this process, closes its ends as it reads the EOFs.
	after := fds()
	for i := 0; i < 100 && after > before; i++ {
		time.Sleep(10 * time.Millisecond)
		after = fds()
	}
	if after > before { // fewer is the warm-up's agent end closing
		t.Errorf("open descriptors went from %d to %d", before, after)
	}
}
