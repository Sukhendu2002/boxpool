package daemon

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
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

// validNodeField keeps what a joining node reports about itself printable.
var validNodeField = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`).MatchString

// PoolHandler is the head's API for other machines, served with
// auth.HeadTLS: POST /v1/nodes joins with a token, and every other
// endpoint needs a client certificate from the head CA.
func PoolHandler(info Info, st *store.Store, ca *x509.Certificate, caKey crypto.Signer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/nodes", registerNode(st, ca, caKey))
	mux.Handle("GET /v1/nodes", requireNode(listNodes(info, st)))
	return mux
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
func registerNode(st *store.Store, ca *x509.Certificate, caKey crypto.Signer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		for _, f := range []string{req.Name, req.OS, req.Arch} {
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
		// Only now, so a malformed request does not burn the token.
		err = st.UseJoinToken(r.Context(), tok.Hash(), time.Now())
		if errors.Is(err, store.ErrTokenInvalid) || errors.Is(err, store.ErrTokenExpired) {
			writeError(w, http.StatusForbidden, err)
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		n, err := st.CreateNode(context.WithoutCancel(r.Context()), store.Node{Name: req.Name, OS: req.OS, Arch: req.Arch})
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
}

// listNodes lists the head and the nodes that joined it.
func listNodes(info Info, st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if head, err := auth.Head(info.StateDir); err != nil || head != "" {
			writeJoined(w, head, err, "run boxpool ls --nodes on the head")
			return
		}
		host, _ := os.Hostname()
		list := []Node{{Name: host, Role: "head", OS: info.OS, Arch: info.Arch}}
		nodes, err := st.ListNodes(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		for _, n := range nodes {
			list = append(list, Node{ID: n.ID, Name: n.Name, Role: "node", OS: n.OS, Arch: n.Arch, JoinedAt: n.JoinedAt})
		}
		writeJSON(w, list)
	}
}

// notJoined returns true if this machine has not joined another pool, and
// otherwise writes a 409 that tells the user what to do instead.
func (s *server) notJoined(w http.ResponseWriter, instead string) bool {
	head, err := auth.Head(s.info.StateDir)
	if err != nil || head != "" {
		writeJoined(w, head, err, instead)
		return false
	}
	return true
}

func writeJoined(w http.ResponseWriter, head string, err error, instead string) {
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeError(w, http.StatusConflict, fmt.Errorf("this machine is a node in the pool at %s: %s", head, instead))
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.notJoined(w, "it can be in only one") {
		return
	}
	// Its nodes would be left with a head that no longer serves them.
	if nodes, err := s.st.ListNodes(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	} else if len(nodes) > 0 {
		writeError(w, http.StatusConflict, fmt.Errorf("this machine is the head of a pool with %d nodes and cannot join another", len(nodes)))
		return
	}
	host, err := os.Hostname()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	id, err := auth.Join(r.Context(), tok, s.info.StateDir, auth.JoinRequest{Name: host, OS: s.info.OS, Arch: s.info.Arch})
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("join the pool at %s: %w", tok.Addr, err))
		return
	}
	slog.Info("joined a pool", "head", tok.Addr, "id", id)
	writeJSON(w, JoinPoolResponse{ID: id, Head: tok.Addr})
}

// servePool serves PoolHandler on auth.JoinPort, creating the head CA
// first if there is none, unless this machine joined another pool. The
// returned stop shuts it down.
func servePool(info Info, st *store.Store) (stop func(), err error) {
	if head, err := auth.Head(info.StateDir); err != nil || head != "" {
		return func() {}, err
	}
	ca, key, err := auth.LoadOrCreateCA(info.StateDir)
	if err != nil {
		return nil, fmt.Errorf("head CA: %w", err)
	}
	cfg, err := auth.HeadTLS(ca, key)
	if err != nil {
		return nil, err
	}
	l, err := tls.Listen("tcp", ":"+auth.JoinPort, cfg)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: PoolHandler(info, st, ca, key), ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(l)
	slog.Info("serving the pool API", "addr", l.Addr())
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if srv.Shutdown(ctx) != nil {
			srv.Close()
		}
	}, nil
}
