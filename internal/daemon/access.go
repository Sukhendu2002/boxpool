package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"boxpool.run/boxpool/internal/auth"
	"boxpool.run/boxpool/internal/store"
)

// AccessTokenRequest is the body of POST /v1/access-tokens. Scope is one
// of auth.Scopes; Sandbox, if set, is the only sandbox the token reaches.
// A zero TTL never expires.
type AccessTokenRequest struct {
	Scope   string        `json:"scope"`
	Sandbox string        `json:"sandbox,omitempty"`
	TTL     time.Duration `json:"ttl_ns,omitempty"`
}

// AccessToken is an access token as GET /v1/access-tokens lists it.
// Token, the secret, is set only in the answer to its creation. Revoke it
// with DELETE /v1/access-tokens/{id}.
type AccessToken struct {
	ID        string    `json:"id"`
	Token     string    `json:"token,omitempty"`
	Scope     string    `json:"scope"`
	Sandbox   string    `json:"sandbox,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	LastUsed  time.Time `json:"last_used,omitzero"`
}

func accessToken(t store.AccessToken) AccessToken {
	return AccessToken{ID: t.ID(), Scope: t.Scope, Sandbox: t.Sandbox, CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt, LastUsed: t.LastUsed}
}

// revokeCheck is how often a remote request that is still running, such
// as a shell, checks that its token is still valid.
var revokeCheck = 2 * time.Second

// createAccessToken issues an access token for this head's API on the
// pool port, creating the head CA first if there is none.
func (s *server) createAccessToken(w http.ResponseWriter, r *http.Request) {
	var req AccessTokenRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("bad request body: %w", err))
		return
	}
	if !slices.Contains(auth.Scopes, req.Scope) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid scope %q: use %s", req.Scope, strings.Join(auth.Scopes, ", ")))
		return
	}
	if req.Sandbox != "" && !validName(req.Sandbox) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid sandbox name %q", req.Sandbox))
		return
	}
	if req.TTL < 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid ttl %s: must be positive", req.TTL))
		return
	}
	s.poolMu.Lock()
	defer s.poolMu.Unlock()
	if !s.notJoined(w, "create access tokens on the head") {
		return
	}
	out, err := s.newAccessToken(r.Context(), req.Scope, req.Sandbox, req.TTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(out)
}

// newAccessToken stores a new access token and returns it with its
// secret. The caller must hold poolMu.
func (s *server) newAccessToken(ctx context.Context, scope, sandbox string, ttl time.Duration) (AccessToken, error) {
	ca, _, err := auth.LoadOrCreateCA(s.info.StateDir)
	if err != nil {
		return AccessToken{}, fmt.Errorf("head CA: %w", err)
	}
	tok := auth.NewAccessToken(sha256.Sum256(ca.Raw))
	rec := store.AccessToken{Hash: tok.Hash(), Scope: scope, Sandbox: sandbox, CreatedAt: time.Now().UTC()}
	if ttl > 0 {
		rec.ExpiresAt = rec.CreatedAt.Add(ttl)
	}
	if err := s.st.CreateAccessToken(ctx, rec); err != nil {
		return AccessToken{}, err
	}
	out := accessToken(rec)
	out.Token = tok.String()
	return out, nil
}

func (s *server) listAccessTokens(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.ListAccessTokens(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := []AccessToken{}
	for _, t := range list {
		out = append(out, accessToken(t))
	}
	writeJSON(w, out)
}

func (s *server) revokeAccessToken(w http.ResponseWriter, r *http.Request) {
	err := s.st.RevokeAccessToken(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrTokenInvalid) {
		writeError(w, http.StatusNotFound, errors.New("no access token with that ID"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// remote serves a request on the pool port that carries an access token
// with api, the local API, if the token allows it. A request still
// running when its token is revoked or expires is cancelled within
// revokeCheck.
func (s *server) remote(api http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		at, err := auth.ParseAccessToken(bearer)
		if err != nil {
			writeError(w, http.StatusUnauthorized, err)
			return
		}
		hash := at.Hash()
		tok, err := s.st.UseAccessToken(r.Context(), hash, time.Now())
		if err != nil {
			writeTokenError(w, err)
			return
		}
		ctx, cancel := context.WithCancelCause(r.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			t := time.NewTicker(revokeCheck)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
				if _, err := s.st.UseAccessToken(ctx, hash, time.Now()); errors.Is(err, store.ErrTokenInvalid) || errors.Is(err, store.ErrTokenExpired) {
					cancel(err)
					// Context cancellation alone cannot interrupt a net/http
					// body read. Retire this connection as well as its request.
					http.NewResponseController(w).SetReadDeadline(time.Now())
					return
				}
			}
		}()
		defer func() { cancel(nil); <-done }() // before Run closes the store
		r = r.WithContext(ctx)
		scopeErr := allowed(tok, r)
		if err := context.Cause(ctx); err != nil {
			w.Header().Set("Connection", "close") // do not drain an interrupted upload
			writeTokenError(w, err)
			return
		}
		if scopeErr != nil {
			writeError(w, http.StatusForbidden, scopeErr)
			return
		}
		// Authorization may have waited for the body. Close the gap before
		// the next periodic check by checking again before any API effect.
		if _, err := s.st.UseAccessToken(ctx, hash, time.Now()); err != nil {
			w.Header().Set("Connection", "close")
			writeTokenError(w, err)
			return
		}
		r.Header.Del("Authorization") // not forwarded to nodes
		api.ServeHTTP(w, r)
	}
}

func writeTokenError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrTokenInvalid):
		writeError(w, http.StatusUnauthorized, errors.New("access token is invalid or revoked"))
	case errors.Is(err, store.ErrTokenExpired):
		writeError(w, http.StatusUnauthorized, errors.New("access token expired"))
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}

// allowed returns nil if tok may make the request r on the local API, and
// otherwise why not. cli and api tokens reach sandboxes, snapshots and the
// node list, but not tokens, the pool or labels; one with a sandbox reaches
// only that sandbox. expose tokens reach nothing here.
func allowed(tok store.AccessToken, r *http.Request) error {
	if tok.Scope == "expose" {
		return errors.New("an expose token only opens exposed URLs")
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/v1/"), "/")
	if r.Method == http.MethodGet && len(parts) == 1 && (parts[0] == "health" || parts[0] == "info") {
		return nil
	}
	if tok.Sandbox == "" {
		if parts[0] == "sandboxes" || parts[0] == "snapshots" || r.Method == http.MethodGet && len(parts) == 1 && parts[0] == "nodes" {
			return nil
		}
		return fmt.Errorf("a %s token cannot %s %s", tok.Scope, r.Method, r.URL.Path)
	}
	other := fmt.Errorf("this token reaches only sandbox %s", tok.Sandbox)
	if parts[0] != "sandboxes" {
		return other
	}
	if len(parts) > 1 {
		if name, err := url.PathUnescape(parts[1]); err != nil || name != tok.Sandbox {
			return other
		}
		return nil
	}
	if r.Method != http.MethodPost {
		return other
	}
	// Creating or starting its own sandbox, as boxpool sh does.
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var req CreateRequest
	if json.Unmarshal(body, &req) != nil || req.Name != tok.Sandbox {
		return other
	}
	if req.From != "" {
		// Snapshots are global objects that outlive their source sandbox;
		// its historical name does not grant this token access to them.
		return errors.New("restoring a snapshot requires a token without a sandbox restriction")
	}
	return nil
}
