package daemon

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"time"

	"boxpool.run/boxpool/internal/auth"
	"boxpool.run/boxpool/internal/store"
	"boxpool.run/boxpool/internal/transport"
)

// Node is a machine in the pool as GET /v1/nodes reports it. The head
// lists itself first, with role "head" and no ID.
type Node struct {
	ID       string    `json:"id,omitempty"`
	Name     string    `json:"name"`
	Role     string    `json:"role"`
	OS       string    `json:"os"`
	Arch     string    `json:"arch"`
	JoinedAt time.Time `json:"joined_at,omitzero"`
	// State is ready, or unreachable while the node misses heartbeats.
	// CPUs and MemoryMB are free for new sandboxes, as it last reported.
	State         string    `json:"state"`
	Labels        []string  `json:"labels"`
	CPUs          int       `json:"cpu"`
	MemoryMB      int64     `json:"mem_mb"`
	SeenAt        time.Time `json:"seen_at,omitzero"`
	TotalCPUs     int       `json:"total_cpu,omitempty"`
	TotalMemoryMB int64     `json:"total_mem_mb,omitempty"`
	Warning       string    `json:"warning,omitempty"`
}

// JoinPoolRequest is the body of POST /v1/pool/join.
type JoinPoolRequest struct {
	Token string `json:"token"`
}

// JoinPoolResponse is this node's ID in the pool at Head.
type JoinPoolResponse struct {
	ID   string `json:"id"`
	Head string `json:"head"`
}

// Node names are displayed, not used as DNS labels. Allow full hostnames
// up to 253 characters; OS and architecture identifiers remain short.
var validNodeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`).MatchString

// validNodeField keeps OS and architecture identifiers printable.
var validNodeField = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`).MatchString

// poolHandler is the head's API for other machines, served with
// auth.HeadTLS: POST /v1/nodes joins with a token, and every other
// endpoint needs a client certificate from the head CA. It shares role
// state with this server's local API. A replica that is not the head
// answers only GET /v1/pool/raft.
func (s *server) poolHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/nodes", s.registerNode)
	mux.Handle("GET /v1/nodes", requireNode(s.listNodes))
	mux.Handle("POST /v1/heartbeat", requireNode(s.receiveReport))
	mux.Handle("GET /v1/tunnel", requireNode(s.tunnel))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/pool/raft" {
			requireNode(s.raftStatus)(w, r)
			return
		}
		if head, err := s.poolHead(); err != nil || head != "" {
			writeJoined(w, head, err, "reach the head there instead")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func requireNode(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// VerifyClientCertIfGiven fails the handshake for a bad certificate,
		// so a verified chain means a node of this pool.
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			writeError(w, http.StatusUnauthorized, errors.New("a client certificate from this pool is required"))
			return
		}
		h(w, r)
	}
}

// registerNode consumes a join token and signs the joining node's
// certificate request.
func (s *server) registerNode(w http.ResponseWriter, r *http.Request) {
	var req auth.JoinRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("bad request body: %w", err))
		return
	}
	tok, err := auth.ParseJoinToken(req.Token)
	if err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	if !validNodeName(req.Name) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid node name %q", req.Name))
		return
	}
	for _, f := range []string{req.OS, req.Arch} {
		if !validNodeField(f) {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid node name, OS or arch %q", f))
			return
		}
	}
	csr, err := auth.ParseCSR(req.CSR)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// Registration and role changes must agree on whether this machine
	// is still a head, before consuming a token or adding a node.
	s.poolMu.Lock()
	defer s.poolMu.Unlock()
	if !s.notJoined(w, "join nodes through the head instead") {
		return
	}
	// Only now, so a malformed request does not burn the token.
	ctx := context.WithoutCancel(r.Context())
	err = s.apply(ctx, command{Op: "use", Hash: tok.Hash(), Time: time.Now()})
	if errors.Is(err, store.ErrTokenInvalid) || errors.Is(err, store.ErrTokenExpired) {
		writeError(w, http.StatusForbidden, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// The pool's second machine starts replicating the head's state.
	if s.raft.Load() == nil {
		if err := s.bootstrap(ctx, tok.Addr); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("replicate the head's state: %w", err))
			return
		}
	}
	n := store.Node{ID: store.NewID(), Name: req.Name, OS: req.OS, Arch: req.Arch, JoinedAt: time.Now().UTC()}
	if err := s.apply(ctx, command{Op: "node", Node: &n}); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	ca, caKey, err := auth.LoadOrCreateCA(s.info.StateDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	cert, err := auth.SignNode(ca, caKey, csr, n.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	slog.Info("node joined", "id", n.ID, "name", n.Name, "os", n.OS, "arch", n.Arch)
	writeJSON(w, auth.JoinResponse{ID: n.ID, Cert: cert, CA: ca.Raw})
}

// listNodes lists the head and the nodes that joined it.
func (s *server) listNodes(w http.ResponseWriter, r *http.Request) {
	if head, err := s.poolHead(); err != nil || head != "" {
		writeJoined(w, head, err, "run boxpool ls --nodes on the head")
		return
	}
	self, err := s.report(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	members, err := s.members(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	host, _ := os.Hostname()
	list := []Node{{Name: host, Role: "head", OS: s.info.OS, Arch: s.info.Arch, State: "ready", Labels: self.Labels, CPUs: self.CPUs, MemoryMB: self.MemoryMB, TotalCPUs: self.TotalCPUs, TotalMemoryMB: self.TotalMemoryMB, Warning: self.Warning}}
	for _, m := range members {
		state := "ready"
		if !m.ready {
			state = string(StateUnreachable)
		}
		list = append(list, Node{ID: m.ID, Name: m.Name, Role: "node", OS: m.OS, Arch: m.Arch, JoinedAt: m.JoinedAt,
			State: state, Labels: m.Labels, CPUs: m.CPUs, MemoryMB: m.MemoryMB, SeenAt: m.SeenAt,
			TotalCPUs: m.TotalCPUs, TotalMemoryMB: m.TotalMemoryMB, Warning: m.Warning})
	}
	writeJSON(w, list)
}

// notJoined returns true if this machine has not joined another pool, and
// otherwise writes a 409 that tells the user what to do instead. The
// caller must hold poolMu.
func (s *server) notJoined(w http.ResponseWriter, instead string) bool {
	if s.joining {
		writeError(w, http.StatusConflict, errors.New("this machine is joining another pool; try again after the join finishes"))
		return false
	}
	head, err := s.poolHead()
	if err != nil || head != "" {
		writeJoined(w, head, err, instead)
		return false
	}
	return true
}

func writeJoined(w http.ResponseWriter, head string, err error, instead string) {
	if errors.Is(err, errNoHead) {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeError(w, http.StatusConflict, fmt.Errorf("this machine is a node in the pool at %s: %s", head, instead))
}

// beginJoin reserves the role transition before contacting the new head.
// Registration and token issuance share this lock and check joining.
func (s *server) beginJoin(w http.ResponseWriter, ctx context.Context, tok auth.JoinToken) bool {
	s.poolMu.Lock()
	defer s.poolMu.Unlock()
	if !s.notJoined(w, "it can be in only one") {
		return false
	}
	// Its nodes would be left with a head that no longer serves them.
	if nodes, err := s.members(ctx); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return false
	} else if len(nodes) > 0 {
		writeError(w, http.StatusConflict, fmt.Errorf("this machine is the head of a pool with %d nodes and cannot join another", len(nodes)))
		return false
	}
	ca, _, err := auth.LoadOrCreateCA(s.info.StateDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("head CA: %w", err))
		return false
	}
	if sha256.Sum256(ca.Raw) == tok.CA {
		writeError(w, http.StatusConflict, errors.New("this machine cannot join its own pool"))
		return false
	}
	s.joining = true
	return true
}

// joinPool joins the pool named in the token: the node's key is made here
// and only its certificate request goes to the head.
func (s *server) joinPool(w http.ResponseWriter, r *http.Request) {
	var req JoinPoolRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("bad request body: %w", err))
		return
	}
	tok, err := auth.ParseJoinToken(req.Token)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	host, err := os.Hostname()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !s.beginJoin(w, r.Context(), tok) {
		return
	}
	defer func() {
		s.poolMu.Lock()
		s.joining = false
		s.poolMu.Unlock()
	}()
	id, err := auth.Join(r.Context(), tok, s.info.StateDir, auth.JoinRequest{Name: host, OS: s.info.OS, Arch: s.info.Arch})
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("join the pool at %s: %w", tok.Addr, err))
		return
	}
	slog.Info("joined a pool", "head", tok.Addr, "id", id)
	writeJSON(w, JoinPoolResponse{ID: id, Head: tok.Addr})
}

// servePool serves poolHandler, and raft, on s.poolAddr, creating the
// head CA first if there is none, until closePool. It does nothing if it
// is already serving. The caller must hold poolMu or be starting up.
func (s *server) servePool() error {
	if s.stopPool != nil {
		return nil
	}
	if err := s.loadPoolTLS(); err != nil {
		return err
	}
	raw, err := transport.ForAddress(s.poolAddr).Listen(context.Background(), s.poolAddr)
	if err != nil {
		return err
	}
	l := tls.NewListener(raw, &tls.Config{GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
		return s.poolTLS.Load(), nil
	}})
	srv := &http.Server{Handler: s.poolHandler(), ReadHeaderTimeout: 10 * time.Second,
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){raftProtocol: s.acceptRaft}}
	srv.RegisterOnShutdown(func() { close(s.poolDone) }) // answers waiting tunnels
	go srv.Serve(l)
	slog.Info("serving the pool API", "addr", l.Addr())
	s.stopPool = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if srv.Shutdown(ctx) != nil {
			srv.Close()
		}
	}
	return nil
}

// loadPoolTLS sets the pool port's TLS from the head CA, creating it if
// there is none. A node that becomes a replica serves the pool's CA from
// then on.
func (s *server) loadPoolTLS() error {
	ca, key, err := auth.LoadOrCreateCA(s.info.StateDir)
	if err != nil {
		return fmt.Errorf("head CA: %w", err)
	}
	cfg, err := auth.HeadTLS(ca, key)
	if err != nil {
		return err
	}
	cfg.NextProtos = []string{raftProtocol, "http/1.1"}
	s.poolTLS.Store(cfg)
	return nil
}

// closePool stops raft, then the pool API.
func (s *server) closePool() {
	if err := s.stopRaft(); err != nil {
		slog.Warn("raft shutdown", "err", err)
	}
	s.poolMu.Lock()
	defer s.poolMu.Unlock()
	if s.stopPool != nil {
		s.stopPool()
		s.stopPool = nil
	}
}
