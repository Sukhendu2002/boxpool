//go:build linux && integration

package docker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/agent"
	"boxpool.run/boxpool/internal/backend"
)

// newTestDriver returns a driver for a new state directory, or skips the
// test when no Docker Engine answers, unless BOXPOOL_TEST_REQUIRE_DOCKER=1
// (as in CI) makes that a failure.
func newTestDriver(ctx context.Context, t *testing.T) *Driver {
	d, err := New(ctx, t.TempDir())
	if err != nil {
		if os.Getenv("BOXPOOL_TEST_REQUIRE_DOCKER") == "1" {
			t.Fatal(err)
		}
		t.Skipf("needs a Docker Engine: %v", err)
	}
	t.Logf("%s, API %s", d.e.Info.Version, d.e.base)
	return d
}

// owned lists the containers, networks and volumes labelled for d's state
// directory: what the test may create and must clean up.
func owned(ctx context.Context, t *testing.T, d *Driver) []string {
	t.Helper()
	filters, _ := json.Marshal(map[string][]string{"label": {labelOwner + "=" + d.root}})
	q := url.Values{"filters": {string(filters)}}
	var cs []struct{ Names []string }
	var ns []struct{ Name string }
	var vs struct{ Volumes []struct{ Name string } }
	all := url.Values{"all": {"1"}, "filters": q["filters"]}
	for _, err := range []error{
		d.e.do(ctx, http.MethodGet, "/containers/json", all, nil, &cs),
		d.e.do(ctx, http.MethodGet, "/networks", q, nil, &ns),
		d.e.do(ctx, http.MethodGet, "/volumes", q, nil, &vs),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	var list []string
	for _, c := range cs {
		list = append(list, "container "+strings.Join(c.Names, ","))
	}
	for _, n := range ns {
		list = append(list, "network "+n.Name)
	}
	for _, v := range vs.Volumes {
		list = append(list, "volume "+v.Name)
	}
	return list
}

func randomID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// sandbox is a created sandbox and a client for its agent.
type sandbox struct {
	t  *testing.T
	id string
	c  *agent.Client
}

func newSandbox(ctx context.Context, t *testing.T, d *Driver) *sandbox {
	id := randomID()
	if _, err := d.Create(ctx, id, backend.Spec{Name: "it-" + id, CPUs: 1, MemoryMB: 512}); err != nil {
		t.Fatal(err)
	}
	c := agent.NewClient(func(ctx context.Context) (net.Conn, error) { return d.Dial(ctx, id) })
	t.Cleanup(func() {
		c.CloseIdleConnections()
		if err := d.Remove(context.Background(), id); err != nil {
			t.Errorf("Remove %s: %v", id, err)
		}
	})
	return &sandbox{t, id, c}
}

// try runs a shell script in the sandbox and returns its exit code and
// combined output.
func (s *sandbox) try(ctx context.Context, script string, args ...string) (int, string) {
	s.t.Helper()
	var out bytes.Buffer
	sess, err := s.c.Exec(ctx, agent.ExecRequest{Argv: append([]string{"sh", "-c", script, "sh"}, args...)}, &out, &out)
	if err != nil {
		s.t.Fatalf("exec %q: %v", script, err)
	}
	code, err := sess.Wait()
	if err != nil {
		s.t.Fatalf("exec %q: %v", script, err)
	}
	return code, out.String()
}

func (s *sandbox) run(ctx context.Context, script string, args ...string) string {
	s.t.Helper()
	code, out := s.try(ctx, script, args...)
	if code != 0 {
		s.t.Fatalf("%s: exit %d: %s", script, code, out)
	}
	return out
}

// The perl in the image's perl-base is the only TCP server and client at
// hand; the image has no netcat.
const (
	listen  = `nohup perl -MIO::Socket::INET -e '$l = IO::Socket::INET->new(LocalAddr => "0.0.0.0", LocalPort => 8080, Listen => 5, ReuseAddr => 1) or die "listen: $!"; while ($c = $l->accept) { print $c "hello\n"; close $c }' >/dev/null 2>&1 &`
	connect = `exec perl -MIO::Socket::INET -e '$s = IO::Socket::INET->new(PeerAddr => $ARGV[0], PeerPort => 8080, Timeout => 3) or die "connect: $!\n"; print scalar <$s>' "$1"`
)

type inspected struct {
	HostConfig struct {
		NanoCPUs                     int64 `json:"NanoCpus"`
		Memory, MemorySwap           int64
		CpusetCpus                   string
		CapDrop, CapAdd, SecurityOpt []string
		ReadonlyRootfs, Privileged   bool
		NetworkMode, PidMode         string
		IpcMode, UsernsMode          string
		PortBindings                 map[string]any
		Devices                      []any
		Binds                        []string
	}
	Mounts []struct {
		Type, Source, Destination string
		RW                        bool
	}
	NetworkSettings struct {
		Networks map[string]struct{ IPAddress string }
	}
}

func TestIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	d := newTestDriver(ctx, t)
	if got := owned(ctx, t, d); len(got) != 0 {
		t.Fatalf("a new state directory already owns %q", got)
	}
	t.Cleanup(func() {
		if got := owned(context.Background(), t, d); len(got) != 0 {
			t.Errorf("left behind: %q", got)
		}
		if entries, _ := os.ReadDir(d.root); len(entries) != 0 {
			t.Errorf("left behind in %s: %v", d.root, entries)
		}
	})
	start := time.Now()
	a, b := newSandbox(ctx, t, d), newSandbox(ctx, t, d)
	t.Logf("two Creates, pulling the image if needed: %v", time.Since(start))

	// Hardening, as the engine reports it.
	var in inspected
	if err := d.e.do(ctx, http.MethodGet, "/containers/"+d.name(a.id)+"/json", nil, nil, &in); err != nil {
		t.Fatal(err)
	}
	h := in.HostConfig
	if h.NanoCPUs != 1e9 || h.Memory != 512<<20 || h.MemorySwap != h.Memory || h.CpusetCpus == "" {
		t.Errorf("resource limits not applied: %+v", h)
	}
	t.Logf("CapDrop %v CapAdd %v SecurityOpt %v ReadonlyRootfs %v NetworkMode %s", h.CapDrop, h.CapAdd, h.SecurityOpt, h.ReadonlyRootfs, h.NetworkMode)
	if !slices.Equal(h.CapDrop, []string{"ALL"}) || len(h.CapAdd) != 0 || !slices.Contains(h.SecurityOpt, "no-new-privileges:true") ||
		!h.ReadonlyRootfs || h.Privileged || h.NetworkMode != d.name(a.id) || len(h.PortBindings) != 0 || len(h.Devices) != 0 || len(h.Binds) != 0 ||
		h.PidMode == "host" || h.IpcMode == "host" || h.UsernsMode == "host" {
		t.Errorf("container is not hardened: %+v", h)
	}
	var mounts []string
	for _, m := range in.Mounts {
		mounts = append(mounts, m.Type+" "+m.Source+" "+m.Destination)
	}
	l := sandboxLayout(d.root, a.id)
	if len(in.Mounts) != 2 || !slices.Contains(mounts, "bind "+l.agent+" /run/boxpool") {
		t.Errorf("mounts %q", mounts)
	}

	for _, s := range []*sandbox{a, b} {
		start := time.Now()
		if err := d.Start(ctx, s.id); err != nil {
			t.Fatal(err)
		}
		t.Logf("Start %s until the agent answered: %v", s.id, time.Since(start))
		out := s.run(ctx, `set -eu
test "$(nproc)" -eq 1
if test -r /sys/fs/cgroup/memory.max; then
  test "$(cat /sys/fs/cgroup/memory.max)" -eq 536870912
  set -- $(cat /sys/fs/cgroup/cpu.max); test "$1" -eq "$2"
  cat /sys/fs/cgroup/memory.max /sys/fs/cgroup/cpu.max /sys/fs/cgroup/cpuset.cpus.effective
else
  test "$(cat /sys/fs/cgroup/memory/memory.limit_in_bytes)" -eq 536870912
  test "$(cat /sys/fs/cgroup/cpu/cpu.cfs_quota_us)" -eq "$(cat /sys/fs/cgroup/cpu/cpu.cfs_period_us)"
  cat /sys/fs/cgroup/memory/memory.limit_in_bytes
fi
awk '/^MemTotal:/ {print "host memory view:", $0}' /proc/meminfo`)
		t.Logf("requested 1 CPU and 512 MiB; cgroup limits:\n%s", out)
	}

	// The agent socket: made by the container's root, reachable by the
	// daemon's user (this test) through a directory only it can enter.
	fi, err := os.Stat(l.sock)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	t.Logf("agent socket: %v uid %d; test runs as uid %d", fi.Mode(), st.Uid, os.Geteuid())
	if fi.Mode()&fs.ModeSocket == 0 || fi.Mode().Perm() != 0666 || st.Uid != 0 {
		t.Errorf("agent socket %v owned by %d, want a 0666 socket owned by root", fi.Mode(), st.Uid)
	}
	if fi, err := os.Stat(l.dir); err != nil || fi.Mode().Perm() != 0700 || fi.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		t.Errorf("sandbox directory %s: %v, %v; want 0700 and ours", l.dir, fi.Mode(), err)
	}

	// Inside: root without capabilities or privilege gain, seccomp on, a
	// read-only system, a writable home and tmpfs, nothing of the host's.
	status := a.run(ctx, "id -u; grep -E '^(CapPrm|CapEff|CapBnd|NoNewPrivs|Seccomp):' /proc/self/status; cat /etc/boxpool-image; pwd; echo $HOME")
	t.Logf("inside:\n%s", status)
	for _, want := range []string{"0\n", "CapPrm:\t0000000000000000", "CapEff:\t0000000000000000", "CapBnd:\t0000000000000000", "NoNewPrivs:\t1", "Seccomp:\t2", "\n/root\n/root\n"} {
		if !strings.Contains(status, want) {
			t.Errorf("inside the sandbox: no %q", want)
		}
	}
	for _, path := range []string{"/etc/x", "/usr/x", "/x"} {
		if code, out := a.try(ctx, "touch "+path); code == 0 || !strings.Contains(out, "Read-only file system") {
			t.Errorf("touch %s: exit %d: %s", path, code, out)
		}
	}
	a.run(ctx, "echo kept > /root/persist && echo gone > /tmp/gone && echo gone > /var/tmp/gone && mount | grep -E ' /(tmp|var/tmp|run) .*tmpfs'")
	if out := a.run(ctx, "for p in /var/run/docker.sock /run/docker.sock /dev/kvm; do ! test -e $p || echo present: $p; done"); out != "" {
		t.Errorf("host resources inside the sandbox:\n%s", out)
	}
	t.Logf("mounts:\n%s", a.run(ctx, "cat /proc/self/mounts"))

	// Networks: a's listener answers a itself and the host, never b.
	ip := func(s *sandbox) string {
		var in inspected
		if err := d.e.do(ctx, http.MethodGet, "/containers/"+d.name(s.id)+"/json", nil, nil, &in); err != nil {
			t.Fatal(err)
		}
		return in.NetworkSettings.Networks[d.name(s.id)].IPAddress
	}
	ipA, ipB := ip(a), ip(b)
	t.Logf("a %s, b %s", ipA, ipB)
	if ipA == "" || ipB == "" {
		t.Fatal("a sandbox has no address")
	}
	for _, s := range []*sandbox{a, b} {
		s.run(ctx, listen)
		var out string
		for i := 0; i < 50 && out != "hello\n"; i++ {
			time.Sleep(100 * time.Millisecond)
			_, out = s.try(ctx, connect, ip(s))
		}
		if out != "hello\n" {
			t.Fatalf("%s cannot reach its own listener: %q", s.id, out)
		}
	}
	c, err := net.DialTimeout("tcp", ipA+":8080", 3*time.Second)
	if err != nil {
		t.Fatalf("host to a's listener (positive control): %v", err)
	}
	line, _ := bufio.NewReader(c).ReadString('\n')
	c.Close()
	if line != "hello\n" {
		t.Errorf("host to a's listener: %q", line)
	}
	if code, out := b.try(ctx, connect, ipA); code == 0 || strings.Contains(out, "hello") {
		t.Errorf("b reached a's listener: exit %d: %q", code, out)
	} else {
		t.Logf("b to a: exit %d: %s", code, strings.TrimSpace(out))
	}
	if code, out := a.try(ctx, connect, ipB); code == 0 || strings.Contains(out, "hello") {
		t.Errorf("a reached b's listener: exit %d: %q", code, out)
	}

	// A new driver for the state directory sees both, running, and can talk
	// to their agents.
	d2, err := New(ctx, strings.TrimSuffix(d.root, "/docker"))
	if err != nil {
		t.Fatal(err)
	}
	list, err := d2.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []*sandbox{a, b} {
		if !slices.Contains(list, backend.Sandbox{ID: s.id, Name: "it-" + s.id, State: backend.StateRunning}) {
			t.Errorf("new driver's List %+v lacks %s", list, s.id)
		}
	}
	c2 := agent.NewClient(func(ctx context.Context) (net.Conn, error) { return d2.Dial(ctx, a.id) })
	if _, err := c2.Version(ctx); err != nil {
		t.Errorf("agent through a new driver: %v", err)
	}
	c2.CloseIdleConnections()

	// Another state directory on the engine neither sees nor removes them.
	other := newTestDriver(ctx, t)
	if list, err := other.List(ctx); err != nil || len(list) != 0 {
		t.Errorf("another state directory lists %+v, %v", list, err)
	}
	if err := other.Remove(ctx, a.id); err != nil {
		t.Errorf("Remove from another state directory: %v", err)
	}

	// A duplicate Create, and one with an image that does not exist, fail
	// and leave nothing new behind.
	before := owned(ctx, t, d)
	if _, err := d.Create(ctx, "too-much-memory", backend.Spec{CPUs: 1, MemoryMB: 1 << 30}); err == nil || !strings.Contains(err.Error(), "insufficient memory") {
		t.Fatalf("capacity refusal: %v", err)
	}
	if _, err := d.Create(ctx, a.id, backend.Spec{CPUs: 1, MemoryMB: 256}); err == nil {
		t.Error("duplicate Create succeeded")
	}
	if _, err := d.Create(ctx, "missing-image", backend.Spec{CPUs: 1, MemoryMB: 256, Image: "ghcr.io/sukhendu2002/boxpool-image:does-not-exist"}); err == nil {
		t.Error("Create with a missing image succeeded")
	} else {
		t.Logf("missing image: %v", err)
	}
	if after := owned(ctx, t, d); !slices.Equal(before, after) {
		t.Errorf("failed Creates changed the resources from %q to %q", before, after)
	}
	if _, err := os.Stat(sandboxLayout(d.root, "missing-image").dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("failed Create left its directory: %v", err)
	}
	a.run(ctx, "true") // a survived all of that

	// a's guest swaps its agent socket for a symlink to b's. A plain dial
	// of the path, with the daemon's permissions, reaches b's agent; Dial
	// must refuse. Start below puts a's socket back.
	a.run(ctx, `rm /run/boxpool/agent.sock && ln -s "$1" /run/boxpool/agent.sock`, sandboxLayout(d.root, b.id).sock)
	plain := agent.NewClient(func(ctx context.Context) (net.Conn, error) { return (&net.Dialer{}).DialContext(ctx, "unix", l.sock) })
	if _, err := plain.Version(ctx); err != nil {
		t.Errorf("control: plain dial through a's symlink: %v", err)
	} else {
		t.Log("control: a plain dial through a's symlink reaches b's agent")
	}
	plain.CloseIdleConnections()
	viaDial := agent.NewClient(func(ctx context.Context) (net.Conn, error) { return d.Dial(ctx, a.id) })
	if _, err := viaDial.Version(ctx); err == nil || !strings.Contains(err.Error(), "not a unix socket") {
		t.Errorf("Dial through a's symlink to b's socket: %v, want a refusal", err)
	} else {
		t.Logf("Dial through a's symlink: %v", err)
	}
	b.run(ctx, "true")

	// Stop keeps /root and empties the tmpfs mounts; Start brings the agent
	// back on the same socket path.
	for i := 0; i < 2; i++ {
		if err := d.Stop(ctx, a.id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.c.Version(ctx); err == nil {
		t.Error("agent answers after Stop")
	}
	if err := d.Start(ctx, a.id); err != nil {
		t.Fatal(err)
	}
	if got := a.run(ctx, "cat /root/persist; ls /tmp/gone /var/tmp/gone 2>&1 || true"); !strings.HasPrefix(got, "kept\n") || strings.Count(got, "No such file") != 2 {
		t.Errorf("after Stop and Start: %q", got)
	}

	// Remove kills a running sandbox; the cleanups check the rest.
	if err := d.Remove(ctx, a.id); err != nil {
		t.Fatal(err)
	}
	if list, _ := d.List(ctx); len(list) != 1 || list[0].ID != b.id {
		t.Errorf("after removing a: %+v", list)
	}
}
