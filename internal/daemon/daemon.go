// Package daemon is the node daemon: it owns this machine's sandboxes and
// serves the local HTTP API on a unix socket in the state directory.
package daemon

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"boxpool.run/boxpool/internal/backend"
	"boxpool.run/boxpool/internal/store"
)

// SocketName is the API socket's file name inside the state directory.
const SocketName = "boxpool.sock"

// StoreName is the sandbox database's file name inside the state directory.
const StoreName = "state.db"

// LockName is the daemon lock's file name inside the state directory; the
// running daemon holds it open.
const LockName = "daemon.lock"

// Defaults for a new sandbox's resources (#13).
const (
	DefaultCPUs     = backend.DefaultCPUs
	DefaultMemoryMB = backend.DefaultMemoryMB
	DefaultDiskGB   = backend.DefaultDiskGB
)

// Info describes the daemon and the node it runs on.
type Info struct {
	Version string `json:"version"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	// StateDir holds the snapshots, in snapshots/; Run sets it.
	StateDir string `json:"state_dir"`
	Backend  string `json:"backend"`
	// PID is the serving daemon's process ID; Run sets it.
	PID int `json:"pid"`
	// Warning, if set, is about the backend, e.g. that it is a fallback with
	// weaker isolation. Commands that create sandboxes print it.
	Warning string `json:"warning,omitempty"`
}

// Driver is a backend that can also connect to the agent in a running
// sandbox, as both drivers do.
type Driver interface {
	backend.Driver
	Dial(ctx context.Context, id string) (net.Conn, error)
}

// CreateRequest is the body of POST /v1/sandboxes. Zero fields get a
// generated name and the default resources.
type CreateRequest struct {
	Name     string `json:"name,omitempty"`
	CPUs     int    `json:"cpu,omitempty"`
	MemoryMB int    `json:"mem_mb,omitempty"`
	DiskGB   int    `json:"disk_gb,omitempty"`
	// Restart is the restart policy of a new sandbox: "always" boots it
	// again when the daemon starts and finds it stopped though it was
	// running, as after a host reboot. "" or "no" means never.
	Restart string `json:"restart,omitempty"`
	// From is a snapshot to copy a new sandbox's disk from. Its disk is at
	// least the snapshot's size.
	From string `json:"from,omitempty"`
}

// SnapshotRequest is the body of POST /v1/sandboxes/{name}/snapshot.
type SnapshotRequest struct {
	Name string `json:"name"`
}

// Snapshot is a named copy of a sandbox's disk, which new sandboxes on the
// same backend can start from. Sandbox is the source's name when it was
// taken.
type Snapshot struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Backend   string    `json:"backend"`
	Sandbox   string    `json:"sandbox"`
	DiskGB    int       `json:"disk_gb"`
	CreatedAt time.Time `json:"created_at"`
	// Warning is set when the sandbox, stopped for the snapshot, did not
	// shut down cleanly, so the snapshot may be inconsistent.
	Warning string `json:"warning,omitempty"`
}

// StateMissing is the state of a sandbox whose record this node has but
// whose backend no longer knows it, e.g. after its container was deleted by
// hand. Removing it deletes the record.
const StateMissing backend.State = "missing"

// Sandbox is a sandbox as the API reports it. State is the backend's live
// state when the sandbox is on this daemon's backend.
type Sandbox struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Backend   string        `json:"backend"`
	State     backend.State `json:"state"`
	CPUs      int           `json:"cpu"`
	MemoryMB  int           `json:"mem_mb"`
	DiskGB    int           `json:"disk_gb"`
	CreatedAt time.Time     `json:"created_at"`
	Restart   string        `json:"restart,omitempty"`
	// Warning is set by stop and remove when the sandbox did not shut down
	// cleanly (backend.ErrUncleanStop): it is stopped, but its disk may be
	// inconsistent. Pause sets it when it stopped the sandbox instead.
	Warning string `json:"warning,omitempty"`
}

// Error is the body of every API error.
type Error struct {
	Message string `json:"message"`
}

// validName keeps names usable as DNS labels, file names and URL path
// segments.
var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`).MatchString

type server struct {
	st        *store.Store
	d         Driver     // nil when this node has no usable backend
	mu        sync.Mutex // serialises changing sandboxes and snapshots
	snapshots string     // directory of the snapshot files, named by ID
}

// Handler returns the daemon's HTTP API. Without a driver, the sandbox
// endpoints answer 503.
func Handler(info Info, st *store.Store, d Driver) http.Handler {
	s := &server{st: st, d: d, snapshots: filepath.Join(info.StateDir, "snapshots")}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, info)
	})
	mux.HandleFunc("POST /v1/sandboxes", s.backendOnly(s.create))
	mux.HandleFunc("GET /v1/sandboxes", s.backendOnly(s.list))
	mux.HandleFunc("GET /v1/sandboxes/{name}", s.backendOnly(s.get))
	mux.HandleFunc("POST /v1/sandboxes/{name}/start", s.backendOnly(s.start))
	mux.HandleFunc("POST /v1/sandboxes/{name}/stop", s.backendOnly(s.stop))
	mux.HandleFunc("POST /v1/sandboxes/{name}/pause", s.backendOnly(s.pause))
	mux.HandleFunc("POST /v1/sandboxes/{name}/resume", s.backendOnly(s.resume))
	mux.HandleFunc("DELETE /v1/sandboxes/{name}", s.backendOnly(s.remove))
	mux.HandleFunc("POST /v1/sandboxes/{name}/snapshot", s.backendOnly(s.snapshot))
	mux.HandleFunc("GET /v1/snapshots", s.backendOnly(s.listSnapshots))
	mux.HandleFunc("DELETE /v1/snapshots/{name}", s.backendOnly(s.removeSnapshot))
	mux.HandleFunc("GET /v1/sandboxes/{name}/exec", s.backendOnly(s.exec))
	mux.HandleFunc("GET /v1/sandboxes/{name}/files", s.backendOnly(s.files))
	mux.HandleFunc("PUT /v1/sandboxes/{name}/files", s.backendOnly(s.files))
	return mux
}

func (s *server) backendOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.d == nil {
			writeError(w, http.StatusServiceUnavailable, errors.New("this node has no usable sandbox backend: run boxpool doctor"))
			return
		}
		h(w, r)
	}
}

// create returns the sandbox with the requested name, started, creating it
// first if there is none, from the snapshot req.From if set.
func (s *server) create(w http.ResponseWriter, r *http.Request) {
	var req CreateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("bad request body: %w", err))
		return
	}
	if req.Name != "" && !validName(req.Name) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid sandbox name %q: use 1 to 63 lower-case letters, digits and hyphens, starting with a letter or digit", req.Name))
		return
	}
	switch req.Restart {
	case "", "no":
		req.Restart = ""
	case "always":
	default:
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid restart policy %q: use always or no", req.Restart))
		return
	}
	spec, err := (backend.Spec{Name: req.Name, CPUs: req.CPUs, MemoryMB: req.MemoryMB, DiskGB: req.DiskGB}).WithDefaults()
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	ctx := r.Context()
	s.mu.Lock()
	defer s.mu.Unlock()
	// A generated name must not attach to an existing sandbox.
	for req.Name == "" {
		b := make([]byte, 3)
		rand.Read(b)
		name := "sb-" + hex.EncodeToString(b)
		if _, err := s.st.GetByName(ctx, name); errors.Is(err, store.ErrNotFound) {
			req.Name = name
		} else if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	spec.Name = req.Name
	if req.From != "" {
		snap, ok := s.lookupSnapshot(w, r, req.From)
		if !ok {
			return
		}
		if req.DiskGB != 0 && req.DiskGB < snap.DiskGB {
			writeError(w, http.StatusBadRequest, fmt.Errorf("snapshot %q needs a disk of at least %d GiB", snap.Name, snap.DiskGB))
			return
		}
		spec.DiskGB = max(spec.DiskGB, snap.DiskGB)
		spec.From = s.snapshotPath(snap)
	}
	sb, err := s.st.GetByName(ctx, req.Name)
	status := http.StatusOK
	switch {
	case err == nil && req.From != "":
		writeError(w, http.StatusConflict, fmt.Errorf("sandbox %q already exists: a snapshot can only start a new sandbox", req.Name))
		return
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusCreated
		sb, err = s.st.Create(ctx, store.Sandbox{Backend: s.d.Name(), Spec: spec, State: backend.StateStopped, Restart: req.Restart})
		if err == nil {
			if _, err = s.d.Create(ctx, sb.ID, spec); err != nil {
				s.st.Delete(context.WithoutCancel(ctx), sb.ID)
			}
		}
	case err == nil:
		if err := s.sameBackend(sb); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// Start does nothing if it runs, and the record can be stale: the VM
	// may have died since.
	if err := s.d.Start(ctx, sb.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.st.SetState(ctx, sb.ID, backend.StateRunning); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	sb.State = backend.StateRunning
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(apiSandbox(sb))
}

func (s *server) get(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.lookup(w, r)
	if !ok {
		return
	}
	list, err := s.live(r.Context(), []store.Sandbox{sb})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, list[0])
}

// list returns every sandbox in the store, oldest first, so a restarted
// daemon still reports the sandboxes an earlier one created.
func (s *server) list(w http.ResponseWriter, r *http.Request) {
	records, err := s.st.List(r.Context())
	if err == nil {
		var list []Sandbox
		if list, err = s.live(r.Context(), records); err == nil {
			writeJSON(w, list)
			return
		}
	}
	writeError(w, http.StatusInternalServerError, err)
}

// live returns records as the API reports them, with the driver's live
// state for those on its backend; the store's state can be stale.
func (s *server) live(ctx context.Context, records []store.Sandbox) ([]Sandbox, error) {
	running, err := s.d.List(ctx)
	if err != nil {
		return nil, err
	}
	state := make(map[string]backend.State, len(running))
	for _, sb := range running {
		state[sb.ID] = sb.State
	}
	list := make([]Sandbox, 0, len(records))
	for _, sb := range records {
		if sb.Backend == s.d.Name() {
			sb.State = cmp.Or(state[sb.ID], StateMissing)
		}
		list = append(list, apiSandbox(sb))
	}
	return list, nil
}

// start boots a stopped sandbox from its kept disk. Starting a running
// sandbox does nothing.
func (s *server) start(w http.ResponseWriter, r *http.Request) { s.boot(w, r, false) }

// resume restores a paused sandbox, or boots one that pause had to stop.
// Resuming a running sandbox does nothing.
func (s *server) resume(w http.ResponseWriter, r *http.Request) { s.boot(w, r, true) }

func (s *server) boot(w http.ResponseWriter, r *http.Request, resume bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sb, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if err := s.sameBackend(sb); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	start := s.d.Start
	if resume {
		list, err := s.live(r.Context(), []store.Sandbox{sb})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		// Only a paused sandbox needs Resume, which Docker never supports;
		// Start boots one that pause had to stop and leaves a running one.
		if list[0].State == backend.StatePaused {
			start = s.d.Resume
		}
	}
	if err := start(r.Context(), sb.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.st.SetState(r.Context(), sb.ID, backend.StateRunning); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	sb.State = backend.StateRunning
	writeJSON(w, apiSandbox(sb))
}

// stop shuts the sandbox down, keeping its disk and its reservation.
func (s *server) stop(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sb, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if err := s.sameBackend(sb); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	warning, err := s.stopSandbox(r.Context(), sb)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	sb.State = backend.StateStopped
	out := apiSandbox(sb)
	out.Warning = warning
	writeJSON(w, out)
}

// pause saves a running sandbox's memory and stops its VM, so it uses no
// CPU or memory until resume. A backend that cannot save memory (Docker,
// or a VM vz cannot save) stops the sandbox instead, with a warning.
func (s *server) pause(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s.mu.Lock()
	defer s.mu.Unlock()
	sb, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if err := s.sameBackend(sb); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	state, warning := backend.StatePaused, ""
	err := s.d.Pause(ctx, sb.ID)
	// vz's fallback stops the VM, cleanly or not, before it reports either.
	if errors.Is(err, backend.ErrNotSupported) || errors.Is(err, backend.ErrUncleanStop) {
		state, warning = backend.StateStopped, fmt.Sprintf("%v: stopped the sandbox instead, so its memory is lost; resume boots it from its disk", err)
		var unclean string
		if unclean, err = s.stopSandbox(ctx, sb); unclean != "" {
			warning += "; " + unclean
		}
	}
	if err == nil {
		err = s.st.SetState(context.WithoutCancel(ctx), sb.ID, state)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	sb.State = state
	out := apiSandbox(sb)
	out.Warning = warning
	writeJSON(w, out)
}

// remove deletes the sandbox: its process, disk, network and reservation
// through the driver, then its record. It refuses a running sandbox unless
// the force query parameter is true, which stops it first.
func (s *server) remove(w http.ResponseWriter, r *http.Request) {
	force, _ := strconv.ParseBool(r.URL.Query().Get("force"))
	ctx := r.Context()
	s.mu.Lock()
	defer s.mu.Unlock()
	sb, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if err := s.sameBackend(sb); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	list, err := s.live(ctx, []store.Sandbox{sb})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := list[0]
	if out.State == backend.StateRunning || out.State == backend.StatePaused {
		if !force {
			writeError(w, http.StatusConflict, fmt.Errorf("sandbox %q is %s: stop it first, or force the removal", sb.Spec.Name, out.State))
			return
		}
		if out.Warning, err = s.stopSandbox(ctx, sb); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if err := s.d.Remove(ctx, sb.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// The sandbox is gone: a cancelled request must not leave its record.
	if err := s.st.Delete(context.WithoutCancel(ctx), sb.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, out)
}

// snapshot copies the sandbox's disk to a new named snapshot. A running
// sandbox is stopped for the copy, so its disk is consistent, and started
// again: its processes and memory are lost. A paused one is refused, so
// that its memory is not.
func (s *server) snapshot(w http.ResponseWriter, r *http.Request) {
	var req SnapshotRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("bad request body: %w", err))
		return
	}
	if !validName(req.Name) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid snapshot name %q: use 1 to 63 lower-case letters, digits and hyphens, starting with a letter or digit", req.Name))
		return
	}
	ctx := r.Context()
	s.mu.Lock()
	defer s.mu.Unlock()
	sb, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if err := s.sameBackend(sb); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	if _, err := s.st.GetSnapshot(ctx, req.Name); err == nil {
		writeError(w, http.StatusConflict, fmt.Errorf("snapshot name %q %w", req.Name, store.ErrNameTaken))
		return
	} else if !errors.Is(err, store.ErrSnapshotNotFound) {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	list, err := s.live(ctx, []store.Sandbox{sb})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	state := list[0].State
	if state == backend.StatePaused {
		writeError(w, http.StatusConflict, fmt.Errorf("sandbox %q is paused: resume or stop it first", sb.Spec.Name))
		return
	}
	var warning string
	if state == backend.StateRunning {
		if warning, err = s.stopSandbox(ctx, sb); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	snap, err := s.st.CreateSnapshot(ctx, store.Snapshot{Name: req.Name, Backend: sb.Backend, Sandbox: sb.Spec.Name, DiskGB: sb.Spec.DiskGB})
	if err == nil {
		if err = os.MkdirAll(s.snapshots, 0700); err == nil {
			err = s.d.Snapshot(ctx, sb.ID, s.snapshotPath(snap))
		}
		if err != nil {
			err = errors.Join(err, s.st.DeleteSnapshot(context.WithoutCancel(ctx), snap.ID))
		}
	}
	if state == backend.StateRunning {
		// Whatever became of the snapshot, the sandbox was running.
		ctx := context.WithoutCancel(ctx)
		startErr := s.d.Start(ctx, sb.ID)
		if startErr == nil {
			startErr = s.st.SetState(ctx, sb.ID, backend.StateRunning)
		}
		err = errors.Join(err, startErr)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := apiSnapshot(snap)
	out.Warning = warning
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(out)
}

// listSnapshots returns every snapshot in the store, oldest first.
func (s *server) listSnapshots(w http.ResponseWriter, r *http.Request) {
	records, err := s.st.ListSnapshots(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	list := make([]Snapshot, 0, len(records))
	for _, snap := range records {
		list = append(list, apiSnapshot(snap))
	}
	writeJSON(w, list)
}

// removeSnapshot deletes the snapshot's file, then its record. Sandboxes
// started from it have their own copy and are not affected.
func (s *server) removeSnapshot(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, ok := s.lookupSnapshot(w, r, r.PathValue("name"))
	if !ok {
		return
	}
	// The file first: a record without one only fails to start sandboxes,
	// and removing it again succeeds.
	if err := os.Remove(s.snapshotPath(snap)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.st.DeleteSnapshot(context.WithoutCancel(ctx), snap.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, apiSnapshot(snap))
}

// lookupSnapshot returns the named snapshot of this daemon's backend, or
// writes a 404 or a 409.
func (s *server) lookupSnapshot(w http.ResponseWriter, r *http.Request, name string) (store.Snapshot, bool) {
	snap, err := s.st.GetSnapshot(r.Context(), name)
	switch {
	case errors.Is(err, store.ErrSnapshotNotFound):
		writeError(w, http.StatusNotFound, fmt.Errorf("snapshot %q not found", name))
	case err != nil:
		writeError(w, http.StatusInternalServerError, err)
	case snap.Backend != s.d.Name():
		writeError(w, http.StatusConflict, fmt.Errorf("snapshot %q is on the %s backend, but this daemon runs %s", name, snap.Backend, s.d.Name()))
	default:
		return snap, true
	}
	return snap, false
}

func (s *server) snapshotPath(snap store.Snapshot) string {
	return filepath.Join(s.snapshots, snap.ID)
}

func apiSnapshot(snap store.Snapshot) Snapshot {
	return Snapshot{ID: snap.ID, Name: snap.Name, Backend: snap.Backend, Sandbox: snap.Sandbox, DiskGB: snap.DiskGB, CreatedAt: snap.CreatedAt}
}

// stopSandbox stops sb and records it. An unclean stop still stops it, so
// it is returned as a warning rather than an error.
func (s *server) stopSandbox(ctx context.Context, sb store.Sandbox) (warning string, err error) {
	err = s.d.Stop(ctx, sb.ID)
	if errors.Is(err, backend.ErrUncleanStop) {
		warning, err = err.Error(), nil
	}
	if err != nil {
		return "", err
	}
	return warning, s.st.SetState(context.WithoutCancel(ctx), sb.ID, backend.StateStopped)
}

// reconcile makes the store match the backend when the daemon starts. A
// sandbox recorded running whose VM is gone died with the host or an
// earlier daemon: it is booted again if its restart policy is always, else
// recorded stopped. One that runs though recorded stopped, as after a crash
// in the middle of a start, is recorded running. A failed restart is logged
// and leaves the sandbox stopped. Sandboxes the backend has but the store
// does not are logged and left alone: deleting them could destroy a disk.
func reconcile(ctx context.Context, st *store.Store, d Driver) error {
	records, err := st.List(ctx)
	if err != nil {
		return err
	}
	live, err := d.List(ctx)
	if err != nil {
		return err
	}
	state := make(map[string]backend.State, len(live))
	for _, sb := range live {
		state[sb.ID] = sb.State
	}
	for _, sb := range records {
		if sb.Backend != d.Name() {
			continue
		}
		got, ok := state[sb.ID]
		delete(state, sb.ID)
		want := got
		switch {
		case !ok:
			slog.Warn("sandbox is missing from the backend", "sandbox", sb.Spec.Name, "id", sb.ID)
			continue
		case got == backend.StateStopped && sb.State == backend.StateRunning && sb.Restart == "always":
			slog.Info("restarting sandbox", "sandbox", sb.Spec.Name)
			if err := d.Start(ctx, sb.ID); err != nil {
				slog.Error("restarting sandbox failed", "sandbox", sb.Spec.Name, "err", err)
				want = backend.StateStopped
			} else {
				want = backend.StateRunning
			}
		}
		if want != sb.State {
			if err := st.SetState(ctx, sb.ID, want); err != nil {
				return err
			}
		}
	}
	for id := range state {
		slog.Warn("backend has a sandbox with no record; leaving it", "id", id)
	}
	return nil
}

// exec relays an agent exec session unchanged (docs/agent-protocol.md)
// between the client's WebSocket and the sandbox's agent.
func (s *server) exec(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if err := s.sameBackend(sb); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	// Dial the agent first, so a stopped sandbox is a plain HTTP error.
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return s.d.Dial(ctx, sb.ID) },
	}}
	ag, _, err := websocket.Dial(r.Context(), "ws://agent/v1/exec", &websocket.DialOptions{HTTPClient: hc})
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("cannot reach the agent in sandbox %q (is it running?): %w", sb.Spec.Name, err))
		return
	}
	defer ag.CloseNow()
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept has written the HTTP error
	}
	defer c.CloseNow()
	relay(r.Context(), c, ag)
}

// files relays a request to the agent's file API (docs/agent-protocol.md)
// unchanged, streaming the tar both ways. A download that fails after its
// status was sent is aborted, so the client cannot mistake it for a whole
// tar.
func (s *server) files(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if err := s.sameBackend(sb); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	p := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = &url.URL{Scheme: "http", Host: "agent", Path: "/v1/files", RawQuery: pr.In.URL.RawQuery}
			pr.Out.Host = "agent"
		},
		Transport: &http.Transport{
			DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return s.d.Dial(ctx, sb.ID) },
			DisableKeepAlives: true,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeError(w, http.StatusBadGateway, fmt.Errorf("cannot reach the agent in sandbox %q (is it running?): %w", sb.Spec.Name, err))
		},
	}
	p.ServeHTTP(w, r)
}

// relay copies messages both ways until either side closes or fails, then
// closes the other: with the same status after a close handshake, at once
// after a failure. The agent kills the command when its side drops.
func relay(ctx context.Context, a, b *websocket.Conn) {
	done := make(chan struct{}, 2)
	pump := func(dst, src *websocket.Conn) {
		defer func() { done <- struct{}{} }()
		for {
			typ, msg, err := src.Read(ctx)
			if err != nil {
				if status := websocket.CloseStatus(err); status != -1 {
					dst.Close(status, "")
				} else {
					dst.CloseNow()
				}
				return
			}
			if err := dst.Write(ctx, typ, msg); err != nil {
				src.CloseNow()
				return
			}
		}
	}
	go pump(a, b)
	go pump(b, a)
	<-done
	<-done
}

// lookup returns the sandbox named in the path, or writes a 404.
func (s *server) lookup(w http.ResponseWriter, r *http.Request) (store.Sandbox, bool) {
	name := r.PathValue("name")
	sb, err := s.st.GetByName(r.Context(), name)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, fmt.Errorf("sandbox %q not found", name))
		return sb, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return sb, false
	}
	return sb, true
}

func (s *server) sameBackend(sb store.Sandbox) error {
	if sb.Backend != s.d.Name() {
		return fmt.Errorf("sandbox %q is on the %s backend, but this daemon runs %s", sb.Spec.Name, sb.Backend, s.d.Name())
	}
	return nil
}

func apiSandbox(sb store.Sandbox) Sandbox {
	return Sandbox{ID: sb.ID, Name: sb.Spec.Name, Backend: sb.Backend, State: sb.State,
		CPUs: sb.Spec.CPUs, MemoryMB: sb.Spec.MemoryMB, DiskGB: sb.Spec.DiskGB, CreatedAt: sb.CreatedAt, Restart: sb.Restart}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(Error{Message: err.Error()})
}
