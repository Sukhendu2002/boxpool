// Package daemon is the node daemon: it owns this machine's sandboxes and
// serves the local HTTP API on a unix socket in the state directory.
package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
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

// Defaults for a new sandbox's resources (#13).
const (
	DefaultCPUs     = backend.DefaultCPUs
	DefaultMemoryMB = backend.DefaultMemoryMB
	DefaultDiskGB   = backend.DefaultDiskGB
)

// Info describes the daemon and the node it runs on.
type Info struct {
	Version  string `json:"version"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	StateDir string `json:"state_dir"`
	Backend  string `json:"backend"`
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
}

// Sandbox is a sandbox as the API reports it.
type Sandbox struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Backend   string        `json:"backend"`
	State     backend.State `json:"state"`
	CPUs      int           `json:"cpu"`
	MemoryMB  int           `json:"mem_mb"`
	DiskGB    int           `json:"disk_gb"`
	CreatedAt time.Time     `json:"created_at"`
}

// Error is the body of every API error.
type Error struct {
	Message string `json:"message"`
}

// validName keeps names usable as DNS labels, file names and URL path
// segments.
var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`).MatchString

type server struct {
	st *store.Store
	d  Driver     // nil when this node has no usable backend
	mu sync.Mutex // serialises creating and starting sandboxes
}

// Handler returns the daemon's HTTP API. Without a driver, the sandbox
// endpoints answer 503.
func Handler(info Info, st *store.Store, d Driver) http.Handler {
	s := &server{st: st, d: d}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, info)
	})
	mux.HandleFunc("POST /v1/sandboxes", s.backendOnly(s.create))
	mux.HandleFunc("GET /v1/sandboxes/{name}", s.backendOnly(s.get))
	mux.HandleFunc("GET /v1/sandboxes/{name}/exec", s.backendOnly(s.exec))
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
// first if there is none.
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
	sb, err := s.st.GetByName(ctx, req.Name)
	status := http.StatusOK
	switch {
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusCreated
		sb, err = s.st.Create(ctx, store.Sandbox{Backend: s.d.Name(), Spec: spec, State: backend.StateStopped})
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
	// Start does nothing if it runs, and the record can be stale: a daemon
	// restart stops Firecracker VMs.
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
	if sb, ok := s.lookup(w, r); ok {
		writeJSON(w, apiSandbox(sb))
	}
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
		CPUs: sb.Spec.CPUs, MemoryMB: sb.Spec.MemoryMB, DiskGB: sb.Spec.DiskGB, CreatedAt: sb.CreatedAt}
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
