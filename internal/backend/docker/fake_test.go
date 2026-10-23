package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"boxpool.run/boxpool/internal/agent"
	"boxpool.run/boxpool/internal/resources"
)

// fakeEngine is the part of the Docker Engine API the driver uses, in
// memory. Starting a container serves a real agent on the socket in its
// bind mount, as the image's command would.
type fakeEngine struct {
	t           *testing.T
	mu          sync.Mutex
	apiVersion  string
	images      map[string]bool // references the engine has
	pullable    map[string]bool // fromImage + " " + tag that a pull finds
	pulls       []string
	volumes     map[string]map[string]string // name → labels
	networks    map[string]fakeNetwork       // name → network
	containers  map[string]*fakeContainer    // name → container
	fail        map[string]int               // "METHOD /path" → status to answer with
	lose        map[string]bool              // "METHOD /path" → handle it, then drop the connection unanswered
	hook        func(method, path string)    // called before each request is handled
	requests    []string
	noAgent     bool // started containers serve no agent
	exitOnStart bool // started containers exit at once
}

type fakeNetwork struct {
	id     string
	labels map[string]string
}

type fakeContainer struct {
	id      string
	cfg     containerConfig
	running bool
	exit    int
	stop    context.CancelFunc // stops the agent
	done    chan struct{}
}

func newFakeEngine(t *testing.T) *fakeEngine {
	return &fakeEngine{
		t: t, apiVersion: "1.48",
		images:     map[string]bool{},
		pullable:   map[string]bool{"ghcr.io/sukhendu2002/boxpool-image " + strings.SplitN(DefaultImage, "@", 2)[1]: true},
		volumes:    map[string]map[string]string{},
		networks:   map[string]fakeNetwork{},
		containers: map[string]*fakeContainer{},
		fail:       map[string]int{},
		lose:       map[string]bool{},
	}
}

// shortTemp is a directory with a short path, for unix sockets: macOS
// allows only 104 bytes.
func shortTemp(t *testing.T) string {
	dir, err := os.MkdirTemp("/tmp", "bp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	dir, _ = filepath.EvalSymlinks(dir)
	return dir
}

// serve serves f on a unix socket and returns its path.
func (f *fakeEngine) serve() string {
	sock := filepath.Join(shortTemp(f.t), "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		f.t.Fatal(err)
	}
	srv := &http.Server{Handler: f}
	go srv.Serve(l)
	f.t.Cleanup(func() {
		srv.Close()
		f.mu.Lock()
		for _, c := range f.containers {
			f.halt(c)
		}
		f.mu.Unlock()
	})
	return sock
}

// driver returns a driver for a new state directory on f.
func (f *fakeEngine) driver(ctx context.Context, sock string) *Driver {
	e, err := dialEngine(ctx, sock)
	if err != nil {
		f.t.Fatal(err)
	}
	d, err := newDriver(e, filepath.Join(shortTemp(f.t), "docker"))
	if err != nil {
		f.t.Fatal(err)
	}
	d.admission.Probe = func(string) (resources.Capacity, error) {
		return resources.Capacity{CPUs: []int{0, 1, 2, 3, 4, 5, 6, 7}, MemoryMB: 32768, AvailableMB: 32768, DiskBytes: 1 << 40}, nil
	}
	return d
}

func (f *fakeEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path != "/version" {
		v, rest, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
		if v != "v"+min(f.apiVersion, maxAPI) {
			f.t.Errorf("%s %s: want API version %s", r.Method, path, maxAPI)
		}
		path = "/" + rest
	}
	f.mu.Lock()
	hook := f.hook
	f.mu.Unlock()
	if hook != nil {
		defer hook(r.Method, path) // after unlocking, before the answer goes out
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+path)
	if f.lose[r.Method+" "+path] {
		conn := w.(http.Hijacker)
		defer func() {
			c, _, err := conn.Hijack()
			if err == nil {
				c.Close()
			}
		}()
		w = httptest.NewRecorder()
	}
	if status, ok := f.fail[r.Method+" "+path]; ok {
		errorf(w, status, "injected failure")
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	switch {
	case path == "/version":
		json.NewEncoder(w).Encode(engineVersion{Version: "28.0.0", APIVersion: f.apiVersion, MinAPIVersion: "1.24", Os: "linux"})
	case r.Method == "GET" && parts[0] == "images" && parts[len(parts)-1] == "json":
		if !f.images[strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/json")] {
			errorf(w, 404, "No such image")
		}
	case r.Method == "POST" && path == "/images/create":
		q := r.URL.Query()
		f.pulls = append(f.pulls, q.Get("fromImage")+" "+q.Get("tag"))
		if !f.pullable[q.Get("fromImage")+" "+q.Get("tag")] {
			fmt.Fprintf(w, `{"status":"Pulling"}`+"\n"+`{"errorDetail":{"message":"manifest unknown"},"error":"manifest unknown"}`+"\n")
			return
		}
		sep := ":"
		if strings.HasPrefix(q.Get("tag"), "sha256:") {
			sep = "@"
		}
		f.images[q.Get("fromImage")+sep+q.Get("tag")] = true
		fmt.Fprintln(w, `{"status":"Downloaded"}`)
	case r.Method == "POST" && path == "/volumes/create":
		var v struct {
			Name   string
			Labels map[string]string
		}
		json.NewDecoder(r.Body).Decode(&v)
		if _, ok := f.volumes[v.Name]; !ok { // like Docker: an existing volume is returned as is
			f.volumes[v.Name] = v.Labels
		}
		json.NewEncoder(w).Encode(map[string]any{"Name": v.Name, "Labels": f.volumes[v.Name]})
	case parts[0] == "volumes" && len(parts) == 2:
		labels, ok := f.volumes[parts[1]]
		switch {
		case !ok:
			errorf(w, 404, "no such volume")
		case r.Method == "GET":
			json.NewEncoder(w).Encode(map[string]any{"Name": parts[1], "Labels": labels})
		case r.Method == "DELETE":
			for _, c := range f.containers {
				for _, m := range c.cfg.HostConfig.Mounts {
					if m.Type == "volume" && m.Source == parts[1] {
						errorf(w, 409, "volume is in use")
						return
					}
				}
			}
			delete(f.volumes, parts[1])
			w.WriteHeader(204)
		}
	case r.Method == "POST" && path == "/networks/create":
		var n struct {
			Name   string
			Labels map[string]string
		}
		json.NewDecoder(r.Body).Decode(&n)
		if _, ok := f.networks[n.Name]; ok {
			errorf(w, 409, "network with name %s already exists", n.Name)
			return
		}
		f.networks[n.Name] = fakeNetwork{id: "net" + n.Name, labels: n.Labels}
		json.NewEncoder(w).Encode(map[string]string{"Id": "net" + n.Name})
	case parts[0] == "networks" && len(parts) == 2:
		name, n, ok := f.network(parts[1])
		switch {
		case !ok:
			errorf(w, 404, "network %s not found", parts[1])
		case r.Method == "GET":
			json.NewEncoder(w).Encode(map[string]any{"Id": n.id, "Name": name, "Labels": n.labels})
		case r.Method == "DELETE":
			for _, c := range f.containers {
				if c.cfg.HostConfig.NetworkMode == name {
					errorf(w, 403, "network has active endpoints")
					return
				}
			}
			delete(f.networks, name)
			w.WriteHeader(204)
		}
	case r.Method == "POST" && path == "/containers/create":
		name := r.URL.Query().Get("name")
		if _, ok := f.containers[name]; ok {
			errorf(w, 409, "Conflict. The container name %q is already in use", "/"+name)
			return
		}
		var cfg containerConfig
		json.NewDecoder(r.Body).Decode(&cfg)
		if !f.images[cfg.Image] {
			errorf(w, 404, "No such image: %s", cfg.Image)
			return
		}
		if _, ok := f.networks[cfg.HostConfig.NetworkMode]; !ok {
			errorf(w, 404, "network %s not found", cfg.HostConfig.NetworkMode)
			return
		}
		f.containers[name] = &fakeContainer{id: "id" + name, cfg: cfg}
		json.NewEncoder(w).Encode(map[string]string{"Id": "id" + name})
	case r.Method == "GET" && path == "/containers/json":
		var filters map[string][]string
		json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters)
		list := []any{}
		for name, c := range f.containers {
			if k, v, _ := strings.Cut(filters["label"][0], "="); c.cfg.Labels[k] != v {
				continue
			}
			state := "exited"
			if c.running {
				state = "running"
			}
			list = append(list, map[string]any{"Names": []string{"/" + name}, "Labels": c.cfg.Labels, "State": state})
		}
		json.NewEncoder(w).Encode(list)
	case parts[0] == "containers" && len(parts) >= 2:
		name, c := f.container(parts[1])
		if c == nil {
			errorf(w, 404, "No such container: %s", parts[1])
			return
		}
		switch r.Method + " " + strings.Join(parts[2:], "/") {
		case "GET json":
			var out container
			out.ID, out.Name, out.Config.Labels = c.id, "/"+name, c.cfg.Labels
			out.HostConfig = c.cfg.HostConfig
			out.State.Running, out.State.ExitCode = c.running, c.exit
			json.NewEncoder(w).Encode(out)
		case "POST start":
			if c.running {
				w.WriteHeader(304)
				return
			}
			f.start(c)
			w.WriteHeader(204)
		case "POST stop":
			if !c.running {
				w.WriteHeader(304)
				return
			}
			f.halt(c)
			w.WriteHeader(204)
		case "DELETE ":
			if c.running && r.URL.Query().Get("force") != "1" {
				errorf(w, 409, "container is running")
				return
			}
			f.halt(c)
			delete(f.containers, name)
			w.WriteHeader(204)
		default:
			errorf(w, 404, "page not found")
		}
	default:
		errorf(w, 404, "page not found")
	}
}

func errorf(w http.ResponseWriter, status int, format string, args ...any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"message": fmt.Sprintf(format, args...)})
}

func (f *fakeEngine) network(ref string) (string, fakeNetwork, bool) {
	for name, n := range f.networks {
		if name == ref || n.id == ref {
			return name, n, true
		}
	}
	return "", fakeNetwork{}, false
}

func (f *fakeEngine) container(ref string) (string, *fakeContainer) {
	for name, c := range f.containers {
		if name == ref || c.id == ref {
			return name, c
		}
	}
	return "", nil
}

// start serves an agent on the socket in the container's bind mount.
func (f *fakeEngine) start(c *fakeContainer) {
	c.running = true
	if f.exitOnStart {
		c.running, c.exit = false, 127
		return
	}
	if f.noAgent {
		return
	}
	var dir string
	for _, m := range c.cfg.HostConfig.Mounts {
		if m.Type == "bind" && m.Target == agentDir {
			dir = m.Source
		}
	}
	l, err := agent.Listen("unix:" + filepath.Join(dir, "agent.sock"))
	if err != nil {
		f.t.Errorf("fake container: %v", err)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.stop, c.done = cancel, make(chan struct{})
	go func() {
		defer close(c.done)
		agent.Serve(ctx, l, "fake")
	}()
}

func (f *fakeEngine) halt(c *fakeContainer) {
	if c.stop != nil {
		c.stop()
		<-c.done
		c.stop = nil
	}
	c.running = false
}

// count returns how many requests matched method and path.
func (f *fakeEngine) count(req string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r == req {
			n++
		}
	}
	return n
}
