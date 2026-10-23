package docker

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/agent"
	"boxpool.run/boxpool/internal/backend"
)

var spec = backend.Spec{Name: "web", CPUs: 1, MemoryMB: 256}

func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	f := newFakeEngine(t)
	sock := f.serve()
	d := f.driver(ctx, sock)
	name := d.name("abc")

	sb, err := d.Create(ctx, "abc", spec)
	if err != nil {
		t.Fatal(err)
	}
	if want := (backend.Sandbox{ID: "abc", Name: "web", State: backend.StateStopped}); sb != want {
		t.Errorf("Create returned %+v, want %+v", sb, want)
	}
	if len(f.pulls) != 1 {
		t.Errorf("pulls %q, want the default image once", f.pulls)
	}
	l := sandboxLayout(d.root, "abc")
	for _, c := range []struct {
		path string
		mode os.FileMode
	}{{l.dir, 0700}, {l.agent, 0777}} {
		if fi, err := os.Stat(c.path); err != nil || fi.Mode().Perm() != c.mode {
			t.Errorf("%s: %v, %v; want mode %v", c.path, fi.Mode(), err, c.mode)
		}
	}
	for _, labels := range []map[string]string{f.volumes[name], f.networks[name].labels, f.containers[name].cfg.Labels} {
		if !d.owns(labels, "abc") {
			t.Errorf("resource labels %v", labels)
		}
	}

	if err := d.Start(ctx, "abc"); err != nil {
		t.Fatal(err)
	}
	if err := d.Start(ctx, "abc"); err != nil {
		t.Errorf("second Start: %v", err)
	}
	c := agent.NewClient(func(ctx context.Context) (net.Conn, error) { return d.Dial(ctx, "abc") })
	defer c.CloseIdleConnections()
	if v, err := c.Version(ctx); err != nil || v.Agent != "fake" {
		t.Errorf("agent version through Dial: %+v, %v", v, err)
	}
	listIs(t, d, backend.Sandbox{ID: "abc", Name: "web", State: backend.StateRunning})

	// A new driver for the same state directory finds the running sandbox
	// and leaves it running.
	d2, err := newDriver(d.e, d.root)
	if err != nil {
		t.Fatal(err)
	}
	listIs(t, d2, backend.Sandbox{ID: "abc", Name: "web", State: backend.StateRunning})
	if _, err := c.Version(ctx); err != nil {
		t.Errorf("agent after a new driver: %v", err)
	}

	for i := 0; i < 2; i++ {
		if err := d2.Stop(ctx, "abc"); err != nil {
			t.Fatalf("Stop %d: %v", i, err)
		}
	}
	listIs(t, d, backend.Sandbox{ID: "abc", Name: "web", State: backend.StateStopped})
	if err := d.Start(ctx, "abc"); err != nil {
		t.Fatalf("Start after Stop: %v", err)
	}
	if f.count("POST /images/create") != 1 {
		t.Error("image pulled again")
	}

	for i := 0; i < 2; i++ {
		if err := d.Remove(ctx, "abc"); err != nil {
			t.Fatalf("Remove %d: %v", i, err)
		}
	}
	if len(f.containers)+len(f.networks)+len(f.volumes) != 0 {
		t.Errorf("Remove left %v %v %v", f.containers, f.networks, f.volumes)
	}
	if _, err := os.Stat(l.dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Remove left %s: %v", l.dir, err)
	}
	listIs(t, d)
	if err := d.Start(ctx, "abc"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Start after Remove: %v, want fs.ErrNotExist", err)
	}
	if err := d.Stop(ctx, "abc"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stop after Remove: %v, want fs.ErrNotExist", err)
	}
	if !errors.Is(d.Pause(ctx, "abc"), backend.ErrNotSupported) || !errors.Is(d.Resume(ctx, "abc"), backend.ErrNotSupported) {
		t.Error("Pause and Resume should be unsupported")
	}
}

func listIs(t *testing.T, d *Driver, want ...backend.Sandbox) {
	t.Helper()
	got, err := d.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) || len(want) > 0 && got[0] != want[0] {
		t.Errorf("List: %+v, want %+v", got, want)
	}
}

func TestCreateRefuses(t *testing.T) {
	ctx := context.Background()
	f := newFakeEngine(t)
	d := f.driver(ctx, f.serve())
	if _, err := d.Create(ctx, "abc", spec); err != nil {
		t.Fatal(err)
	}
	before := len(f.requests)
	for _, c := range []struct {
		id   string
		spec backend.Spec
	}{
		{"abc", spec}, // a duplicate
		{"", spec}, {"..", spec}, {"a/b", spec},
		{"x", backend.Spec{MemoryMB: 256}},
		{"x", backend.Spec{CPUs: 1}},
		{"x", backend.Spec{CPUs: 1, MemoryMB: 1, Image: "debian"}}, // the mutable latest tag
		{strings.Repeat("x", 64), spec},                            // too long a socket path below a long root
	} {
		d := d
		if len(c.id) == 64 {
			d = &Driver{e: d.e, root: "/" + strings.Repeat("r", 40), prefix: d.prefix}
		}
		if _, err := d.Create(ctx, c.id, c.spec); err == nil {
			t.Errorf("Create(%q, %+v) succeeded", c.id, c.spec)
		}
	}
	if len(f.requests) != before {
		t.Errorf("refused Creates sent %q", f.requests[before:])
	}
	if _, ok := f.containers[d.name("abc")]; !ok {
		t.Error("the duplicate Create removed the first sandbox")
	}
}

// A resource left with a sandbox's name, by anyone, is never adopted or
// removed.
func TestCreateExisting(t *testing.T) {
	ctx := context.Background()
	f := newFakeEngine(t)
	d := f.driver(ctx, f.serve())
	for _, kind := range []string{"volume", "network"} {
		name := d.name(kind)
		if kind == "volume" {
			f.volumes[name] = labels(d.root, kind) // even with this driver's labels
		} else {
			f.networks[name] = fakeNetwork{id: "n1", labels: map[string]string{"other": "yes"}}
		}
		_, err := d.Create(ctx, kind, spec)
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Errorf("Create over an existing %s: %v", kind, err)
		}
		if _, err := os.Stat(sandboxLayout(d.root, kind).dir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("failed Create left its directory: %v", err)
		}
	}
	if len(f.volumes) != 1 || len(f.networks) != 1 || len(f.containers) != 0 {
		t.Errorf("resources after refused Creates: %v %v %v", f.volumes, f.networks, f.containers)
	}
}

func TestCreateRollback(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(f *fakeEngine, cancel context.CancelFunc)
	}{
		{"pull fails", func(f *fakeEngine, _ context.CancelFunc) { f.pullable = nil }},
		{"volume fails", func(f *fakeEngine, _ context.CancelFunc) { f.fail["POST /volumes/create"] = 500 }},
		{"network fails", func(f *fakeEngine, _ context.CancelFunc) { f.fail["POST /networks/create"] = 500 }},
		{"container fails", func(f *fakeEngine, _ context.CancelFunc) { f.fail["POST /containers/create"] = 500 }},
		{"cancelled during container create", func(f *fakeEngine, cancel context.CancelFunc) {
			// The caller gives up once the engine has the container: the
			// cleanup must still remove it, its network and its volume.
			f.hook = func(method, path string) {
				if method+" "+path == "POST /containers/create" {
					cancel()
					time.Sleep(50 * time.Millisecond)
				}
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := newFakeEngine(t)
			d := f.driver(context.Background(), f.serve())
			c.setup(f, cancel)
			if _, err := d.Create(ctx, "abc", spec); err == nil {
				t.Fatal("Create succeeded")
			} else {
				t.Log(err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.containers)+len(f.networks)+len(f.volumes) != 0 {
				t.Errorf("failed Create left %v %v %v", f.containers, f.networks, f.volumes)
			}
			if _, err := os.Stat(sandboxLayout(d.root, "abc").dir); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("failed Create left its directory: %v", err)
			}
		})
	}
}

// Resources named like a sandbox's but labelled for another owner or
// sandbox are refused by every operation and left alone.
func TestForeignResources(t *testing.T) {
	ctx := context.Background()
	f := newFakeEngine(t)
	sock := f.serve()
	d := f.driver(ctx, sock)
	if _, err := d.Create(ctx, "abc", spec); err != nil {
		t.Fatal(err)
	}
	name := d.name("abc")
	for _, lb := range []map[string]string{
		{labelOwner: "/elsewhere", labelSandbox: "abc"},
		{labelOwner: d.root, labelSandbox: "abd"},
		{labelOwner: d.root},
		nil,
	} {
		f.containers[name].cfg.Labels = lb
		for op, fn := range map[string]func(context.Context, string) error{"Start": d.Start, "Stop": d.Stop, "Remove": d.Remove} {
			if err := fn(ctx, "abc"); !errors.Is(err, errForeign) {
				t.Errorf("%s with container labels %v: %v, want errForeign", op, lb, err)
			}
		}
		listIs(t, d)
	}
	if len(f.containers) != 1 || f.containers[name].running {
		t.Error("foreign container was changed")
	}
	// The container is gone, but the network is someone else's.
	delete(f.containers, name)
	f.networks[name] = fakeNetwork{id: "n1", labels: map[string]string{labelOwner: "/elsewhere", labelSandbox: "abc"}}
	if err := d.Remove(ctx, "abc"); !errors.Is(err, errForeign) {
		t.Errorf("Remove with a foreign network: %v", err)
	}
	if len(f.networks) != 1 || len(f.volumes) != 1 {
		t.Error("Remove deleted a foreign network, or the volume before it")
	}
	f.networks[name] = fakeNetwork{id: "n1", labels: labels(d.root, "abc")}
	f.volumes[name] = map[string]string{labelOwner: d.root, labelSandbox: "other"}
	if err := d.Remove(ctx, "abc"); !errors.Is(err, errForeign) {
		t.Errorf("Remove with a foreign volume: %v", err)
	}
	if len(f.networks) != 0 || len(f.volumes) != 1 {
		t.Errorf("networks %v, volumes %v; want only the foreign volume", f.networks, f.volumes)
	}
}

// Two state directories on one engine never see or touch each other's
// sandboxes, even with the same IDs.
func TestStateDirsIndependent(t *testing.T) {
	ctx := context.Background()
	f := newFakeEngine(t)
	sock := f.serve()
	a, b := f.driver(ctx, sock), f.driver(ctx, sock)
	if a.name("x") == b.name("x") {
		t.Fatalf("both state directories name sandbox x %s", a.name("x"))
	}
	for _, d := range []*Driver{a, b} {
		if _, err := d.Create(ctx, "x", spec); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Start(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	if err := b.Remove(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	listIs(t, b)
	listIs(t, a, backend.Sandbox{ID: "x", Name: "web", State: backend.StateRunning})
	if len(f.containers) != 1 || len(f.networks) != 1 || len(f.volumes) != 1 {
		t.Errorf("after removing b's x: %v %v %v", f.containers, f.networks, f.volumes)
	}
}

func TestStartFailure(t *testing.T) {
	defer func(old time.Duration) { agentTimeout = old }(agentTimeout)
	agentTimeout = 300 * time.Millisecond
	ctx := context.Background()
	for _, c := range []struct {
		name string
		set  func(f *fakeEngine, cid string)
		want string
	}{
		{"exits", func(f *fakeEngine, _ string) { f.exitOnStart = true }, "exited with status 127"},
		{"no agent", func(f *fakeEngine, _ string) { f.noAgent = true }, "waiting for the guest agent"},
		{"engine refuses", func(f *fakeEngine, cid string) { f.fail["POST /containers/"+cid+"/start"] = 500 }, "injected failure"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeEngine(t)
			d := f.driver(ctx, f.serve())
			if _, err := d.Create(ctx, "abc", spec); err != nil {
				t.Fatal(err)
			}
			c.set(f, f.containers[d.name("abc")].id)
			err := d.Start(ctx, "abc")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Start: %v, want %q", err, c.want)
			}
			if f.containers[d.name("abc")].running {
				t.Error("failed Start left the container running")
			}
		})
	}
}

func TestNegotiate(t *testing.T) {
	for _, c := range []struct {
		server, min, want string // want "" for an error
	}{
		{"1.51", "1.24", "1.47"},
		{"1.47", "1.24", "1.47"},
		{"1.43", "1.24", "1.43"},
		{"1.41", "1.12", "1.41"},
		{"1.40", "1.12", ""},
		{"1.60", "1.50", ""},
	} {
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"Version":"x","ApiVersion":"` + c.server + `","MinAPIVersion":"` + c.min + `"}`))
		})}
		sock := filepath.Join(shortTemp(t), "d.sock")
		l, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		go srv.Serve(l)
		e, err := dialEngine(context.Background(), sock)
		srv.Close()
		if c.want == "" {
			if err == nil {
				t.Errorf("server %s (min %s): negotiated %s", c.server, c.min, e.base)
			}
			continue
		}
		if err != nil || e.base != "http://docker/v"+c.want {
			t.Errorf("server %s (min %s): %v, %v; want v%s", c.server, c.min, e, err, c.want)
		}
	}
}

func TestEngineUnreachable(t *testing.T) {
	ctx := context.Background()
	dir := shortTemp(t)
	if _, err := dialEngine(ctx, filepath.Join(dir, "missing.sock")); err == nil || !strings.Contains(err.Error(), "no Docker Engine is listening") {
		t.Errorf("missing socket: %v", err)
	}
	if os.Geteuid() == 0 {
		return // root ignores the socket's mode
	}
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	os.Chmod(sock, 0)
	if _, err := dialEngine(ctx, sock); err == nil || !strings.Contains(err.Error(), "docker group") {
		t.Errorf("socket without permission: %v", err)
	}
}

func TestEngineErrorBody(t *testing.T) {
	ctx := context.Background()
	f := newFakeEngine(t)
	d := f.driver(ctx, f.serve())
	f.fail["GET /containers/json"] = 500
	_, err := d.List(ctx)
	if !statusIs(err, 500) || !strings.Contains(err.Error(), "injected failure") {
		t.Errorf("List: %v", err)
	}
}
