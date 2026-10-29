package docker

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"boxpool.run/boxpool/internal/agent"
	"boxpool.run/boxpool/internal/backend"
	"boxpool.run/boxpool/internal/resources"
)

// Timeouts; tests may shorten them.
var (
	agentTimeout = 30 * time.Second // container start to the agent answering
	stopTimeout  = 10 * time.Second // SIGTERM to SIGKILL in Stop, and each cleanup
)

// Driver is the Docker backend. All its state is in the engine and the
// sandbox directories, so a new Driver sees, and can use, the sandboxes an
// earlier one created, including running ones.
type Driver struct {
	e         *engine
	root      string // <state dir>/docker, symlinks resolved; the owner label
	prefix    string // of every resource name
	admission *resources.Manager
}

var _ backend.Driver = (*Driver)(nil)

// New connects to the Docker Engine (DOCKER_HOST if it is a unix:// URL,
// else /var/run/docker.sock) and creates the docker directory in stateDir.
// It changes nothing in the engine.
func New(ctx context.Context, stateDir string) (*Driver, error) {
	if runtime.GOOS != "linux" {
		// Docker Desktop runs containers in a VM, where a bind-mounted unix
		// socket does not connect to the host.
		return nil, fmt.Errorf("the docker backend needs a Linux host, not %s", runtime.GOOS)
	}
	socket, err := host()
	if err != nil {
		return nil, err
	}
	e, err := dialEngine(ctx, socket)
	if err != nil {
		return nil, err
	}
	d, err := newDriver(e, filepath.Join(stateDir, "docker"))
	if err == nil {
		err = d.adopt(ctx)
	}
	return d, err
}

func newDriver(e *engine, root string) (*Driver, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	state, err := filepath.Abs(filepath.Dir(root))
	if err == nil {
		state, err = filepath.EvalSymlinks(state)
	}
	if err != nil {
		return nil, err
	}
	// Absolute and without symlinks: it is the owner label, so an alias of
	// the state directory must not look like another owner.
	root, err = filepath.Abs(root)
	if err == nil {
		root, err = filepath.EvalSymlinks(root)
	}
	if err != nil {
		return nil, err
	}
	return &Driver{e: e, root: root, prefix: prefix(root), admission: &resources.Manager{StateDir: state}}, nil
}

// Probe reports the Docker Engine's version, or why it cannot be used.
func Probe(ctx context.Context) (string, error) {
	socket, err := host()
	if err != nil {
		return "", err
	}
	e, err := dialEngine(ctx, socket)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Docker %s, API %s, on %s", e.Info.Version, e.Info.APIVersion, socket), nil
}

func (d *Driver) Name() string { return "docker" }

func (d *Driver) name(id string) string { return d.prefix + id }

// Create makes the sandbox's directory, volume, network and container,
// pulling the image if the engine lacks it. Spec.Image, if set, must pin a
// tag or digest; the default is DefaultImage. With Spec.From, /root starts
// as the snapshot's instead of the image's. On failure it removes what it
// made, and never touches resources that already existed.
func (d *Driver) Create(ctx context.Context, id string, spec backend.Spec) (_ backend.Sandbox, err error) {
	if err := checkID(id); err != nil {
		return backend.Sandbox{}, err
	}
	if spec, err = spec.WithDefaults(); err != nil {
		return backend.Sandbox{}, err
	}
	image := spec.Image
	if image == "" {
		image = DefaultImage
	}
	repo, tag, err := splitImage(image)
	if err != nil {
		return backend.Sandbox{}, err
	}
	l := sandboxLayout(d.root, id)
	// sun_path holds 108 bytes including the terminating NUL.
	if len(l.sock) > 107 {
		return backend.Sandbox{}, fmt.Errorf("sandbox %s: socket path %s is longer than 107 bytes: use a shorter state directory", id, l.sock)
	}
	if d.admission == nil {
		return backend.Sandbox{}, fmt.Errorf("resource admission is not initialized")
	}
	unlock, err := d.admission.LockSandbox(ctx, "docker", id)
	if err != nil {
		return backend.Sandbox{}, err
	}
	defer unlock()
	allocation, err := d.admission.Create(ctx, "docker", id, spec, 0)
	if err != nil {
		return backend.Sandbox{}, err
	}
	name := d.name(id)
	var made []func(context.Context) error // undo steps, newest last
	defer func() {
		if err == nil {
			return
		}
		cleanup, cancel := cleanupContext(ctx)
		defer cancel()
		var cleanupErr error
		for i := len(made) - 1; i >= 0; i-- {
			cleanupErr = errors.Join(cleanupErr, made[i](cleanup))
		}
		// A resource may still exist after a failed rollback. Retain its
		// reservation and transport directory until Remove succeeds.
		if cleanupErr == nil {
			cleanupErr = os.RemoveAll(l.dir)
		}
		err = errors.Join(err, cleanupErr)
		err = fmt.Errorf("create sandbox %s: %w", id, err)
	}()
	// The container's root has no CAP_DAC_OVERRIDE, so it needs the
	// permission bits; the 0700 directory above keeps everyone else out.
	if err := os.Mkdir(l.agent, 0700); err != nil {
		return backend.Sandbox{}, err
	}
	if err := os.Chmod(l.agent, 0777); err != nil {
		return backend.Sandbox{}, err
	}
	if err := d.ensureImage(ctx, image, repo, tag); err != nil {
		return backend.Sandbox{}, err
	}
	// Creating a volume that exists returns the existing one, and older
	// engines allow duplicate network names: refuse both up front.
	for _, kind := range []string{"volume", "network", "container"} {
		if err := d.absent(ctx, kind, name); err != nil {
			return backend.Sandbox{}, err
		}
	}
	// Each undo step goes in before its request: even a failed request may
	// have created the resource, if only the answer was lost. The steps
	// check the name and labels, so they never remove another's resource.
	made = append(made, func(ctx context.Context) error { return d.removeVolume(ctx, id) })
	if err := d.e.do(ctx, http.MethodPost, "/volumes/create", nil, map[string]any{"Name": name, "Labels": labels(d.root, id)}, nil); err != nil {
		return backend.Sandbox{}, err
	}
	made = append(made, func(ctx context.Context) error { return d.removeNetwork(ctx, id) })
	err = d.e.do(ctx, http.MethodPost, "/networks/create", nil, map[string]any{
		"Name": name, "Driver": "bridge", "Labels": labels(d.root, id), "CheckDuplicate": true,
	}, nil)
	if err != nil {
		return backend.Sandbox{}, err
	}
	made = append(made, func(ctx context.Context) error { return d.removeContainer(ctx, id) })
	cfg := newContainerConfig(d.root, id, spec, image, name, l)
	cfg.HostConfig.CpusetCpus = allocation.CPUSet()
	var created struct {
		ID string `json:"Id"`
	}
	if err := d.e.do(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name}}, cfg, &created); err != nil {
		return backend.Sandbox{}, err
	}
	if spec.From != "" {
		if err := d.restore(ctx, created.ID, spec.From); err != nil {
			return backend.Sandbox{}, err
		}
	}
	return backend.Sandbox{ID: id, Name: spec.Name, State: backend.StateStopped}, nil
}

// absent returns nil if the engine has no kind (volume, network or
// container) called name.
func (d *Driver) absent(ctx context.Context, kind, name string) error {
	path := "/" + kind + "s/" + name
	if kind == "container" {
		path += "/json"
	}
	err := d.e.do(ctx, http.MethodGet, path, nil, nil, nil)
	switch {
	case statusIs(err, http.StatusNotFound):
		return nil
	case err == nil:
		return fmt.Errorf("%s %s already exists", kind, name)
	}
	return err
}

// ensureImage pulls the image unless the engine has it.
func (d *Driver) ensureImage(ctx context.Context, image, repo, tag string) error {
	err := d.e.do(ctx, http.MethodGet, "/images/"+image+"/json", nil, nil, nil)
	if !statusIs(err, http.StatusNotFound) {
		return err
	}
	return d.e.pull(ctx, repo, tag)
}

// container is the part of GET /containers/{id}/json the driver reads.
type container struct {
	ID         string `json:"Id"`
	Name       string
	Config     struct{ Labels map[string]string }
	HostConfig hostConfig
	State      struct {
		Status   string
		Running  bool
		ExitCode int
		Error    string
	}
}

// adopt accounts for containers created before resource manifests existed.
// It only records exact-name, ownership-verified containers, leaving Engine
// settings and running processes untouched.
func (d *Driver) adopt(ctx context.Context) error {
	list, err := d.List(ctx)
	if err != nil {
		return err
	}
	for _, sb := range list {
		if err := d.adoptSandbox(ctx, sb.ID); err != nil {
			return err
		}
	}
	return nil
}

func (d *Driver) adoptSandbox(ctx context.Context, id string) error {
	if err := checkID(id); err != nil {
		return err
	}
	// Keep Remove from deleting the reservation between inspection and
	// adoption; otherwise the stale inspection could recreate its directory.
	unlock, err := d.admission.LockSandbox(ctx, "docker", id)
	if err != nil {
		return err
	}
	defer unlock()
	c, err := d.inspect(ctx, id)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // removed since List
	}
	if err != nil {
		return err
	}
	h := c.HostConfig
	if h.NanoCPUs < 1 || h.Memory < 1 {
		return fmt.Errorf("sandbox %s has no CPU or memory limit; remove it before creating more sandboxes", id)
	}
	cpus, err := resources.ParseCPUSet(h.CpusetCpus)
	if err != nil {
		return err
	}
	spec := backend.Spec{Name: c.Config.Labels[labelName], CPUs: int((h.NanoCPUs-1)/1_000_000_000 + 1), MemoryMB: int((h.Memory-1)/(1<<20) + 1), DiskGB: backend.DefaultDiskGB}
	if spec, err = spec.WithDefaults(); err != nil {
		return fmt.Errorf("sandbox %s has invalid limits: %w", id, err)
	}
	return d.admission.Adopt(ctx, "docker", id, resources.Allocation{Spec: spec, CPUs: cpus})
}

// errForeign is returned for a resource with a sandbox's name that this
// driver did not create.
var errForeign = errors.New("exists but was not created by this boxpool state directory")

// inspect returns the sandbox's container, an error wrapping
// fs.ErrNotExist if there is none, or one wrapping errForeign if the
// container with its name is not the driver's.
func (d *Driver) inspect(ctx context.Context, id string) (container, error) {
	var c container
	if err := checkID(id); err != nil {
		return c, err
	}
	name := d.name(id)
	err := d.e.do(ctx, http.MethodGet, "/containers/"+name+"/json", nil, nil, &c)
	if statusIs(err, http.StatusNotFound) {
		return c, fmt.Errorf("sandbox %s: %w", id, fs.ErrNotExist)
	}
	if err != nil {
		return c, err
	}
	if c.Name != "/"+name || !d.owns(c.Config.Labels, id) {
		return c, fmt.Errorf("container %s %w", name, errForeign)
	}
	return c, nil
}

func (d *Driver) owns(labels map[string]string, id string) bool {
	return labels[labelOwner] == d.root && labels[labelSandbox] == id
}

// Start starts the container and returns once its agent answers. Starting
// a running sandbox does nothing. If the start request fails or the agent
// does not answer, the container is stopped again: a failed request may
// still have started it, if only the answer was lost.
func (d *Driver) Start(ctx context.Context, id string) error {
	if err := checkID(id); err != nil {
		return err
	}
	unlock, err := d.admission.LockSandbox(ctx, "docker", id)
	if err != nil {
		return err
	}
	defer unlock()
	c, err := d.inspect(ctx, id)
	if err != nil {
		return err
	}
	if c.State.Running {
		return nil
	}
	// By ID, which the inspect checked the owner of: the name could be
	// another container's by now.
	err = d.e.do(ctx, http.MethodPost, "/containers/"+c.ID+"/start", nil, nil, nil)
	if statusIs(err, http.StatusNotModified) {
		err = nil
	}
	if err == nil {
		err = d.waitAgent(ctx, id, c.ID)
	}
	if err != nil {
		cleanup, cancel := cleanupContext(ctx)
		defer cancel()
		err = errors.Join(err, d.stop(cleanup, c.ID, 0))
		return fmt.Errorf("start sandbox %s: %w (see docker logs %s)", id, err, d.name(id))
	}
	return nil
}

// waitAgent polls the agent until it answers, the container stops or
// agentTimeout passes.
func (d *Driver) waitAgent(ctx context.Context, id, cid string) error {
	ctx, cancel := context.WithTimeout(ctx, agentTimeout)
	defer cancel()
	ag := d.agent(id)
	defer ag.CloseIdleConnections()
	for {
		_, err := ag.Version(ctx)
		if err == nil {
			return nil
		}
		var c container
		if ierr := d.e.do(ctx, http.MethodGet, "/containers/"+cid+"/json", nil, nil, &c); ierr == nil && !c.State.Running {
			return fmt.Errorf("container exited with status %d while waiting for the agent: %s", c.State.ExitCode, c.State.Error)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for the guest agent: %w (last error: %v)", ctx.Err(), err)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Stop stops the container: SIGTERM, then SIGKILL after stopTimeout. Files
// under /root stay; the tmpfs mounts are emptied. Stopping a stopped
// sandbox does nothing.
func (d *Driver) Stop(ctx context.Context, id string) error {
	if err := checkID(id); err != nil {
		return err
	}
	unlock, err := d.admission.LockSandbox(ctx, "docker", id)
	if err != nil {
		return err
	}
	defer unlock()
	c, err := d.inspect(ctx, id)
	if err != nil {
		return err
	}
	if err := d.stop(ctx, c.ID, stopTimeout); err != nil {
		return fmt.Errorf("stop sandbox %s: %w", id, err)
	}
	return nil
}

func (d *Driver) stop(ctx context.Context, cid string, grace time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, grace+apiTimeout)
	defer cancel()
	q := url.Values{"t": {fmt.Sprint(int(grace.Seconds()))}}
	err := d.e.do(ctx, http.MethodPost, "/containers/"+cid+"/stop", q, nil, nil)
	if statusIs(err, http.StatusNotModified) {
		return nil
	}
	return err
}

// Remove deletes the sandbox's container, network, volume and directory,
// killing the container if it runs. Each step skips what is already gone,
// so Remove can be repeated after a failure; a resource with the sandbox's
// name that the driver does not own is an error and is kept.
func (d *Driver) Remove(ctx context.Context, id string) error {
	if err := checkID(id); err != nil {
		return err
	}
	unlock, err := d.admission.LockSandbox(ctx, "docker", id)
	if err != nil {
		return err
	}
	defer unlock()
	if err := d.removeContainer(ctx, id); err != nil {
		return fmt.Errorf("remove sandbox %s: %w", id, err)
	}
	// The container is gone: caller cancellation must not strand the rest.
	cleanup, cancel := cleanupContext(ctx)
	defer cancel()
	if err := d.removeNetwork(cleanup, id); err != nil {
		return fmt.Errorf("remove sandbox %s: %w", id, err)
	}
	if err := d.removeVolume(cleanup, id); err != nil {
		return fmt.Errorf("remove sandbox %s: %w", id, err)
	}
	return os.RemoveAll(sandboxLayout(d.root, id).dir)
}

func (d *Driver) removeContainer(ctx context.Context, id string) error {
	c, err := d.inspect(ctx, id)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	err = d.e.do(ctx, http.MethodDelete, "/containers/"+c.ID, url.Values{"force": {"1"}}, nil, nil)
	if statusIs(err, http.StatusNotFound) {
		return nil
	}
	return err
}

func (d *Driver) removeNetwork(ctx context.Context, id string) error {
	name := d.name(id)
	var n struct {
		ID     string `json:"Id"`
		Name   string
		Labels map[string]string
	}
	err := d.e.do(ctx, http.MethodGet, "/networks/"+name, nil, nil, &n)
	if statusIs(err, http.StatusNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if n.Name != name || !d.owns(n.Labels, id) {
		return fmt.Errorf("network %s %w", name, errForeign)
	}
	err = d.e.do(ctx, http.MethodDelete, "/networks/"+n.ID, nil, nil, nil)
	if statusIs(err, http.StatusNotFound) {
		return nil
	}
	return err
}

func (d *Driver) removeVolume(ctx context.Context, id string) error {
	name := d.name(id)
	var v struct {
		Name   string
		Labels map[string]string
	}
	err := d.e.do(ctx, http.MethodGet, "/volumes/"+name, nil, nil, &v)
	if statusIs(err, http.StatusNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if v.Name != name || !d.owns(v.Labels, id) {
		return fmt.Errorf("volume %s %w", name, errForeign)
	}
	err = d.e.do(ctx, http.MethodDelete, "/volumes/"+name, nil, nil, nil)
	if statusIs(err, http.StatusNotFound) {
		return nil
	}
	return err
}

// Pause and Resume are not supported: Docker cannot save a container's
// memory. The daemon stops and starts the sandbox instead.
func (d *Driver) Pause(ctx context.Context, id string) error {
	return fmt.Errorf("pause: %w: docker cannot save a container's memory", backend.ErrNotSupported)
}

func (d *Driver) Resume(ctx context.Context, id string) error {
	return fmt.Errorf("resume: %w: docker cannot save a container's memory", backend.ErrNotSupported)
}

// Snapshot saves /root of a stopped sandbox, the only path that persists,
// to path as a tar of its contents. The root file system is read-only, so
// docker commit would save nothing the guest changed. A running sandbox is
// refused: its files may be mid-write.
func (d *Driver) Snapshot(ctx context.Context, id, path string) (err error) {
	if err := checkID(id); err != nil {
		return err
	}
	unlock, err := d.admission.LockSandbox(ctx, "docker", id)
	if err != nil {
		return err
	}
	defer unlock()
	c, err := d.inspect(ctx, id)
	if err != nil {
		return err
	}
	if c.State.Running {
		return fmt.Errorf("snapshot sandbox %s: it is not stopped", id)
	}
	ctx, cancel := context.WithTimeout(ctx, archiveTimeout)
	defer cancel()
	resp, err := d.e.send(ctx, http.MethodGet, "/containers/"+c.ID+"/archive", url.Values{"path": {"/root"}}, nil)
	if err != nil {
		return fmt.Errorf("snapshot sandbox %s: %w", id, err)
	}
	defer resp.Body.Close()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(path)
			err = fmt.Errorf("snapshot sandbox %s: %w", id, err)
		}
	}()
	err = rebase(f, resp.Body, "root/")
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

// rebase copies the tar in to out with prefix, a directory under / ending
// in a slash, cut from every name, dropping the entry for the prefix
// itself, so it extracts into the prefix's directory.
func rebase(out io.Writer, in io.Reader, prefix string) error {
	tr, tw := tar.NewReader(in), tar.NewWriter(out)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return tw.Close()
		}
		if err != nil {
			return err
		}
		name, ok := strings.CutPrefix(h.Name, prefix)
		if !ok || name == "" {
			continue
		}
		h.Name = name
		switch {
		case h.Typeflag == tar.TypeLink:
			h.Linkname = strings.TrimPrefix(h.Linkname, prefix)
		case h.Typeflag == tar.TypeSymlink && !path.IsAbs(h.Linkname):
			// The engine refuses to extract a relative symlink that leaves
			// the target directory, such as /root/tmp -> ../tmp; the
			// absolute path it resolves to in the container is the same.
			if abs := path.Join("/", prefix, path.Dir(name), h.Linkname); !strings.HasPrefix(abs, "/"+prefix) {
				h.Linkname = abs
			}
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
}

// restore extracts the snapshot at path into /root of the container cid.
func (d *Driver) restore(ctx context.Context, cid, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	ctx, cancel := context.WithTimeout(ctx, archiveTimeout)
	defer cancel()
	resp, err := d.e.send(ctx, http.MethodPut, "/containers/"+cid+"/archive", url.Values{"path": {"/root"}}, f)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// List returns the driver's sandboxes, running or not, found by their
// labels.
func (d *Driver) List(ctx context.Context) ([]backend.Sandbox, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {labelOwner + "=" + d.root}})
	var cs []struct {
		Names  []string
		Labels map[string]string
		State  string
	}
	q := url.Values{"all": {"1"}, "filters": {string(filters)}}
	if err := d.e.do(ctx, http.MethodGet, "/containers/json", q, nil, &cs); err != nil {
		return nil, err
	}
	var list []backend.Sandbox
	for _, c := range cs {
		id := c.Labels[labelSandbox]
		if checkID(id) != nil || !d.owns(c.Labels, id) || !slices.Contains(c.Names, "/"+d.name(id)) {
			continue
		}
		state := backend.StateStopped
		switch c.State {
		case "running":
			state = backend.StateRunning
		case "paused":
			state = backend.StatePaused
		}
		list = append(list, backend.Sandbox{ID: id, Name: c.Labels[labelName], State: state})
	}
	return list, nil
}

// Dial connects to the agent in a running sandbox, for agent.NewClient.
func (d *Driver) Dial(ctx context.Context, id string) (net.Conn, error) {
	if err := checkID(id); err != nil {
		return nil, err
	}
	return dialSocket(ctx, sandboxLayout(d.root, id).sock)
}

func (d *Driver) agent(id string) *agent.Client {
	return agent.NewClient(func(ctx context.Context) (net.Conn, error) { return d.Dial(ctx, id) })
}

func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), stopTimeout+apiTimeout)
}
