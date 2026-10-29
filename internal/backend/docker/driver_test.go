package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
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
	if cid := f.containers[name].id; f.count("POST /containers/"+cid+"/start") != 1 || f.count("POST /containers/"+cid+"/stop") != 0 {
		t.Errorf("second Start sent %q", f.requests)
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
		{"x", backend.Spec{CPUs: -1}},
		{"x", backend.Spec{MemoryMB: -1}},
		{"x", backend.Spec{DiskGB: -1}},
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
	// cancelAt cancels the caller once the engine has handled req, before
	// the answer goes out.
	cancelAt := func(req string) func(*fakeEngine, string, context.CancelFunc) {
		return func(f *fakeEngine, _ string, cancel context.CancelFunc) {
			f.hook = func(method, path string) {
				if method+" "+path == req {
					cancel()
					time.Sleep(50 * time.Millisecond)
				}
			}
		}
	}
	lose := func(req string) func(*fakeEngine, string, context.CancelFunc) {
		return func(f *fakeEngine, _ string, _ context.CancelFunc) { f.lose[req] = true }
	}
	foreign := map[string]string{labelOwner: "/elsewhere", labelSandbox: "abc"}
	for _, c := range []struct {
		name  string
		setup func(f *fakeEngine, name string, cancel context.CancelFunc)
		want  string // in the error, besides the failure itself
		left  int    // resources the rollback must keep
	}{
		{name: "pull fails", setup: func(f *fakeEngine, _ string, _ context.CancelFunc) { f.pullable = nil }},
		{name: "volume fails", setup: func(f *fakeEngine, _ string, _ context.CancelFunc) { f.fail["POST /volumes/create"] = 500 }},
		{name: "network fails", setup: func(f *fakeEngine, _ string, _ context.CancelFunc) { f.fail["POST /networks/create"] = 500 }},
		{name: "container fails", setup: func(f *fakeEngine, _ string, _ context.CancelFunc) { f.fail["POST /containers/create"] = 500 }},
		// The engine makes the resource but the caller gives up, or the
		// answer is lost: the cleanup must still remove it and the rest.
		{name: "cancelled during volume create", setup: cancelAt("POST /volumes/create")},
		{name: "cancelled during network create", setup: cancelAt("POST /networks/create")},
		{name: "cancelled during container create", setup: cancelAt("POST /containers/create")},
		{name: "volume answer lost", setup: lose("POST /volumes/create")},
		{name: "network answer lost", setup: lose("POST /networks/create")},
		{name: "container answer lost", setup: lose("POST /containers/create")},
		// What the cleanup cannot remove stays, and the error says so.
		{name: "volume cleanup fails", setup: func(f *fakeEngine, name string, _ context.CancelFunc) {
			f.lose["POST /volumes/create"] = true
			f.fail["DELETE /volumes/"+name] = 500
		}, want: "injected failure", left: 1},
		{name: "network cleanup fails", setup: func(f *fakeEngine, name string, _ context.CancelFunc) {
			f.lose["POST /networks/create"] = true
			f.fail["DELETE /networks/net"+name] = 500
		}, want: "injected failure", left: 1},
		// Another's resource that appears after the check is never removed.
		{name: "foreign volume appears", setup: func(f *fakeEngine, name string, _ context.CancelFunc) {
			f.fail["POST /volumes/create"] = 500
			f.hook = func(method, path string) {
				if method+" "+path == "GET /volumes/"+name {
					f.mu.Lock()
					f.volumes[name] = foreign
					f.mu.Unlock()
				}
			}
		}, want: errForeign.Error(), left: 1},
		{name: "foreign network appears", setup: func(f *fakeEngine, name string, _ context.CancelFunc) {
			f.hook = func(method, path string) {
				if method+" "+path == "GET /networks/"+name {
					f.mu.Lock()
					f.networks[name] = fakeNetwork{id: "n1", labels: foreign}
					f.mu.Unlock()
				}
			}
		}, want: errForeign.Error(), left: 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := newFakeEngine(t)
			d := f.driver(context.Background(), f.serve())
			c.setup(f, d.name("abc"), cancel)
			_, err := d.Create(ctx, "abc", spec)
			if err == nil {
				t.Fatal("Create succeeded")
			}
			t.Log(err)
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error does not say %q", c.want)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if n := len(f.containers) + len(f.networks) + len(f.volumes); n != c.left {
				t.Errorf("failed Create left %v %v %v, want %d", f.containers, f.networks, f.volumes, c.left)
			}
			if c.want == errForeign.Error() && f.volumes[d.name("abc")][labelOwner] != "/elsewhere" && f.networks[d.name("abc")].labels[labelOwner] != "/elsewhere" {
				t.Errorf("the foreign resource is gone: %v %v", f.volumes, f.networks)
			}
			_, statErr := os.Stat(sandboxLayout(d.root, "abc").dir)
			if c.left == 0 && !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("successful rollback left directory: %v", statErr)
			}
			if c.left != 0 && statErr != nil {
				t.Errorf("failed rollback lost reservation: %v", statErr)
			}
			// Newest first: the container holds the network and the volume.
			var order []string
			for _, r := range f.requests {
				if kind, ok := strings.CutPrefix(r, "DELETE /"); ok {
					order = append(order, strings.SplitN(kind, "/", 2)[0])
				}
			}
			if !slices.IsSortedFunc(order, func(a, b string) int {
				rank := map[string]int{"containers": 0, "networks": 1, "volumes": 2}
				return rank[a] - rank[b]
			}) {
				t.Errorf("cleanup deleted in the order %q", order)
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
	// atStart runs fn once the engine has started the container, before
	// the answer goes out.
	atStart := func(fn func(f *fakeEngine, name string, cancel context.CancelFunc)) func(*fakeEngine, string, string, context.CancelFunc) {
		return func(f *fakeEngine, name, cid string, cancel context.CancelFunc) {
			f.hook = func(method, path string) {
				if method+" "+path == "POST /containers/"+cid+"/start" {
					fn(f, name, cancel)
					time.Sleep(50 * time.Millisecond)
				}
			}
		}
	}
	for _, c := range []struct {
		name    string
		set     func(f *fakeEngine, name, cid string, cancel context.CancelFunc)
		want    string
		running bool // the sandbox's container after the failed Start
	}{
		{name: "exits", set: func(f *fakeEngine, _, _ string, _ context.CancelFunc) { f.exitOnStart = true }, want: "exited with status 127"},
		{name: "no agent", set: func(f *fakeEngine, _, _ string, _ context.CancelFunc) { f.noAgent = true }, want: "waiting for the guest agent"},
		{name: "engine refuses", set: func(f *fakeEngine, _, cid string, _ context.CancelFunc) {
			f.fail["POST /containers/"+cid+"/start"] = 500
		}, want: "injected failure"},
		// The engine starts the container, but the caller gives up or the
		// answer is lost: the container must not be left running.
		{name: "cancelled once started", set: atStart(func(_ *fakeEngine, _ string, cancel context.CancelFunc) { cancel() }), want: "context canceled"},
		{name: "start answer lost", set: func(f *fakeEngine, _, cid string, _ context.CancelFunc) {
			f.lose["POST /containers/"+cid+"/start"] = true
		}, want: "EOF"},
		{name: "stop after a lost answer fails", set: func(f *fakeEngine, _, cid string, _ context.CancelFunc) {
			f.lose["POST /containers/"+cid+"/start"] = true
			f.fail["POST /containers/"+cid+"/stop"] = 500
		}, want: "injected failure", running: true},
		// Meanwhile the name went to another's container: only the one
		// Start inspected, by ID, is stopped.
		{name: "name reused", set: atStart(func(f *fakeEngine, name string, cancel context.CancelFunc) {
			cancel()
			f.mu.Lock()
			f.containers["renamed"] = f.containers[name]
			f.containers[name] = &fakeContainer{id: "foreign", running: true}
			f.mu.Unlock()
		}), want: "context canceled"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := newFakeEngine(t)
			d := f.driver(ctx, f.serve())
			if _, err := d.Create(ctx, "abc", spec); err != nil {
				t.Fatal(err)
			}
			name := d.name("abc")
			cid := f.containers[name].id
			c.set(f, name, cid, cancel)
			err := d.Start(ctx, "abc")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Start: %v, want %q", err, c.want)
			}
			t.Log(err)
			f.mu.Lock()
			defer f.mu.Unlock()
			if _, own := f.container(cid); own.running != c.running {
				t.Errorf("after the failed Start the container runs: %v, want %v", own.running, c.running)
			}
			if other := f.containers[name]; other.id == "foreign" && (!other.running || slices.Contains(f.requests, "POST /containers/foreign/stop")) {
				t.Error("the failed Start touched another's container")
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

// TestSnapshot checks that a sandbox created from a snapshot starts with the
// source's /root, and that a running sandbox is refused.
func TestSnapshot(t *testing.T) {
	ctx := context.Background()
	f := newFakeEngine(t)
	d := f.driver(ctx, f.serve())
	if _, err := d.Create(ctx, "abc", spec); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.containers[d.name("abc")].files = map[string]string{".bashrc": "alias ll='ls -l'", "bin/jq": "jq"}
	f.mu.Unlock()
	path := filepath.Join(t.TempDir(), "snap")
	if err := d.Start(ctx, "abc"); err != nil {
		t.Fatal(err)
	}
	if err := d.Snapshot(ctx, "abc", path); err == nil || !strings.Contains(err.Error(), "not stopped") {
		t.Errorf("Snapshot of a running sandbox = %v", err)
	}
	if err := d.Stop(ctx, "abc"); err != nil {
		t.Fatal(err)
	}
	if err := d.Snapshot(ctx, "abc", path); err != nil {
		t.Fatal(err)
	}
	if err := d.Snapshot(ctx, "abc", path); err == nil {
		t.Error("Snapshot over an existing file succeeded")
	}
	from := spec
	from.From = path
	if _, err := d.Create(ctx, "def", from); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	got := f.containers[d.name("def")].files
	if len(got) != 2 || got[".bashrc"] != "alias ll='ls -l'" || got["bin/jq"] != "jq" {
		t.Errorf("/root from the snapshot = %q", got)
	}
	// The image's /root must not fill the volume under the snapshot's.
	for name, want := range map[string]bool{"abc": false, "def": true} {
		vol := f.containers[d.name(name)].cfg.HostConfig.Mounts[0].VolumeOptions
		if got := vol != nil && vol.NoCopy; got != want {
			t.Errorf("%s: volume NoCopy = %v, want %v", name, got, want)
		}
	}
}

func TestRebase(t *testing.T) {
	var in, out bytes.Buffer
	tw := tar.NewWriter(&in)
	for _, h := range []tar.Header{
		{Typeflag: tar.TypeDir, Name: "root/"},
		{Typeflag: tar.TypeReg, Name: "root/a"},
		{Typeflag: tar.TypeLink, Name: "root/hard", Linkname: "root/a"},
		{Typeflag: tar.TypeSymlink, Name: "root/in", Linkname: "a"},
		{Typeflag: tar.TypeSymlink, Name: "root/d/up", Linkname: "../a"},
		{Typeflag: tar.TypeSymlink, Name: "root/tmp", Linkname: "../tmp"},
		{Typeflag: tar.TypeSymlink, Name: "root/self", Linkname: ".."},
		{Typeflag: tar.TypeSymlink, Name: "root/abs", Linkname: "/etc"},
		{Typeflag: tar.TypeReg, Name: "other"},
	} {
		tw.WriteHeader(&h)
	}
	tw.Close()
	if err := rebase(&out, &in, "root/"); err != nil {
		t.Fatal(err)
	}
	var got []string
	tr := tar.NewReader(&out)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, h.Name+">"+h.Linkname)
	}
	want := []string{"a>", "hard>a", "in>a", "d/up>../a", "tmp>/tmp", "self>/", "abs>/etc"}
	if !slices.Equal(got, want) {
		t.Errorf("rebase = %q, want %q", got, want)
	}
}
