package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"boxpool.run/boxpool/internal/agent"
	"boxpool.run/boxpool/internal/auth"
	"boxpool.run/boxpool/internal/store"
	"boxpool.run/boxpool/internal/transport"
)

// ExposeRequest is the body of POST /v1/exposes.
type ExposeRequest struct {
	Sandbox string `json:"sandbox"`
	Port    int    `json:"port"`
	Public  bool   `json:"public,omitempty"`
}

// Expose is a sandbox port the head serves at URL. Unless it is public,
// URL carries an expose token only in the answer to its creation: opening
// it swaps the token for a cookie. Revoking TokenID closes the URL.
type Expose struct {
	Sandbox   string    `json:"sandbox"`
	Port      int       `json:"port"`
	Public    bool      `json:"public,omitempty"`
	URL       string    `json:"url"`
	TokenID   string    `json:"token_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func apiExpose(e store.Expose) Expose {
	return Expose{Sandbox: e.Sandbox, Port: e.SandboxPort, Public: e.Public, URL: e.URL, TokenID: e.TokenID, CreatedAt: e.CreatedAt}
}

// serveHTTPS and unserveHTTPS put a URL with a trusted certificate in
// front of http://127.0.0.1:port; tests replace them.
var (
	serveHTTPS   = transport.Tailscale{}.Serve
	unserveHTTPS = transport.Tailscale{}.Unserve
)

// tokenParam is the query parameter of an exposed URL that carries its
// token, and cookiePrefix starts the name of the cookie it is swapped for.
const (
	tokenParam   = "boxpool_token"
	cookiePrefix = "boxpool_expose_"
)

// createExpose serves a sandbox's port on a new loopback port of the head,
// behind tailscale serve, with an expose token for it unless it is public.
func (s *server) createExpose(w http.ResponseWriter, r *http.Request) {
	var req ExposeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("bad request body: %w", err))
		return
	}
	if !validName(req.Sandbox) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid sandbox name %q", req.Sandbox))
		return
	}
	if req.Port < 1 || req.Port > 65535 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid port %d", req.Port))
		return
	}
	ctx := r.Context()
	s.poolMu.Lock()
	defer s.poolMu.Unlock()
	if !s.notJoined(w, "expose ports on the head") {
		return
	}
	if _, err := s.st.GetExpose(ctx, req.Sandbox, req.Port); err == nil {
		writeError(w, http.StatusConflict, fmt.Errorf("port %d of sandbox %q is already exposed", req.Port, req.Sandbox))
		return
	}
	if _, err := s.st.GetByName(ctx, req.Sandbox); errors.Is(err, store.ErrNotFound) {
		members, err := s.members(ctx)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if _, ok := owner(members, req.Sandbox); !ok {
			writeError(w, http.StatusNotFound, fmt.Errorf("sandbox %q not found", req.Sandbox))
			return
		}
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// Only tailscale serve, on the same machine, reaches the loopback port.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	e := store.Expose{Port: l.Addr().(*net.TCPAddr).Port, Sandbox: req.Sandbox, SandboxPort: req.Port, Public: req.Public}
	var tok AccessToken
	if !req.Public {
		if tok, err = s.newAccessToken(ctx, "expose", req.Sandbox, 0); err != nil {
			l.Close()
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		e.TokenID = tok.ID
	}
	if e.URL, err = serveHTTPS(ctx, e.Port); err == nil {
		var stored store.Expose
		if stored, err = s.st.CreateExpose(ctx, e); err != nil {
			unserveHTTPS(context.WithoutCancel(ctx), e.Port)
		}
		e = stored
	}
	if err != nil {
		l.Close()
		if tok.ID != "" {
			s.st.RevokeAccessToken(context.WithoutCancel(ctx), tok.ID)
		}
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.startExpose(e, l)
	out := apiExpose(e)
	if tok.Token != "" {
		out.URL += "?" + tokenParam + "=" + tok.Token
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(out)
}

func (s *server) listExposes(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.ListExposes(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := []Expose{}
	for _, e := range list {
		out = append(out, apiExpose(e))
	}
	writeJSON(w, out)
}

// removeExpose stops serving a sandbox port and revokes its token.
func (s *server) removeExpose(w http.ResponseWriter, r *http.Request) {
	port, _ := strconv.Atoi(r.PathValue("port"))
	ctx := context.WithoutCancel(r.Context())
	s.poolMu.Lock()
	defer s.poolMu.Unlock()
	e, err := s.st.GetExpose(ctx, r.PathValue("sandbox"), port)
	if errors.Is(err, store.ErrExposeNotFound) {
		writeError(w, http.StatusNotFound, fmt.Errorf("port %s of sandbox %q is not exposed", r.PathValue("port"), r.PathValue("sandbox")))
		return
	}
	if err == nil {
		// The URL is dead without the listener: a failure must not keep it.
		if err := unserveHTTPS(ctx, e.Port); err != nil {
			slog.Warn("cannot stop tailscale serve for an exposed port", "port", e.Port, "err", err)
		}
	}
	if err == nil && e.TokenID != "" {
		if err = s.st.RevokeAccessToken(ctx, e.TokenID); errors.Is(err, store.ErrTokenInvalid) {
			err = nil // revoked by hand already
		}
	}
	if err == nil {
		err = s.st.DeleteExpose(ctx, e.Port)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if srv := s.exposed[e.Port]; srv != nil {
		srv.Close()
		delete(s.exposed, e.Port)
	}
	writeJSON(w, apiExpose(e))
}

// serveExposes serves the stored exposes again, on the loopback ports
// tailscale serve still sends them to.
func (s *server) serveExposes(ctx context.Context) {
	list, err := s.st.ListExposes(ctx)
	if err != nil {
		slog.Warn("cannot serve exposed ports", "err", err)
		return
	}
	s.poolMu.Lock()
	defer s.poolMu.Unlock()
	for _, e := range list {
		l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(e.Port)))
		if err != nil {
			slog.Warn("cannot serve exposed port; boxpool expose rm and expose it again", "sandbox", e.Sandbox, "port", e.SandboxPort, "err", err)
			continue
		}
		s.startExpose(e, l)
	}
}

// startExpose serves e on l until closeExposes or its removal. The caller
// must hold poolMu.
func (s *server) startExpose(e store.Expose, l net.Listener) {
	if s.exposed == nil {
		s.exposed = map[int]*http.Server{}
	}
	srv := &http.Server{Handler: s.exposeHandler(e), ReadHeaderTimeout: 10 * time.Second}
	s.exposed[e.Port] = srv
	go srv.Serve(l)
}

func (s *server) closeExposes() {
	s.poolMu.Lock()
	defer s.poolMu.Unlock()
	for port, srv := range s.exposed {
		srv.Close()
		delete(s.exposed, port)
	}
}

// exposeHandler checks a request for an exposed URL and sends it, through
// the local API, to the sandbox's port on whichever node has it.
func (s *server) exposeHandler(e store.Expose) http.Handler {
	api := s.api
	if api == nil { // tests serve exposes without Run
		api = s.handler()
	}
	cookie := cookiePrefix + strconv.Itoa(e.Port)
	prefix := "/v1/ports/" + e.Sandbox + "/" + strconv.Itoa(e.SandboxPort)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !e.Public {
			secret, fromQuery := r.URL.Query().Get(tokenParam), true
			if secret == "" {
				fromQuery = false
				if c, err := r.Cookie(cookie); err == nil {
					secret = c.Value
				}
			}
			if err := s.checkExposeToken(r.Context(), e, secret); err != nil {
				http.Error(w, "boxpool: "+err.Error(), http.StatusUnauthorized)
				return
			}
			if fromQuery {
				// Swap the token for a cookie and take it out of the address
				// bar. One leading slash, so the redirect stays on this host.
				http.SetCookie(w, &http.Cookie{Name: cookie, Value: secret, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
				q := r.URL.Query()
				q.Del(tokenParam)
				loc := "/" + strings.TrimLeft(r.URL.EscapedPath(), "/")
				if len(q) > 0 {
					loc += "?" + q.Encode()
				}
				w.Header().Set("Location", loc)
				w.WriteHeader(http.StatusSeeOther)
				return
			}
		}
		// Every exposed URL shares the host, so the browser sends each
		// one's cookie to all of them: none may reach a sandbox.
		cookies := r.Cookies()
		r.Header.Del("Cookie")
		for _, c := range cookies {
			if !strings.HasPrefix(c.Name, cookiePrefix) {
				r.AddCookie(c)
			}
		}
		escaped := r.URL.EscapedPath()
		r.URL.Path, r.URL.RawPath = prefix+r.URL.Path, prefix+escaped
		api.ServeHTTP(w, r)
	})
}

// checkExposeToken returns nil if secret is a valid expose token for e.
//
// shortcut: the token is checked when a request starts, so a WebSocket
// open when it is revoked stays open; check it periodically, as remote
// does, if exposed URLs carry long-lived sessions worth cutting.
func (s *server) checkExposeToken(ctx context.Context, e store.Expose, secret string) error {
	if secret == "" {
		return errors.New("this URL needs the token boxpool expose printed")
	}
	at, err := auth.ParseAccessToken(secret)
	if err != nil {
		return err
	}
	tok, err := s.st.UseAccessToken(ctx, at.Hash(), time.Now())
	if errors.Is(err, store.ErrTokenInvalid) {
		return errors.New("the token is invalid or revoked")
	}
	if err != nil {
		return err
	}
	if tok.Scope != "expose" || tok.Sandbox != "" && tok.Sandbox != e.Sandbox {
		return fmt.Errorf("the token does not open sandbox %s", e.Sandbox)
	}
	return nil
}

// ports proxies a request to a TCP port in the sandbox through its agent
// (docs/agent-protocol.md), WebSocket upgrades included. The path after
// /v1/ports/{name}/{port} is the path the port sees.
func (s *server) ports(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if err := s.sameBackend(sb); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port < 1 || port > 65535 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid port %q", r.PathValue("port")))
		return
	}
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), "/v1/ports/"+sb.Spec.Name+"/"+r.PathValue("port"))
	path, err := url.PathUnescape(rest)
	if !ok || err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("bad path %q", r.URL.EscapedPath()))
		return
	}
	p := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", "localhost:"+strconv.Itoa(port)
			pr.Out.URL.Path, pr.Out.URL.RawPath = path, rest
			// Dev servers refuse Host names they do not expect.
			pr.Out.Host = pr.Out.URL.Host
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				c, err := s.d.Dial(ctx, sb.ID)
				if err != nil {
					return nil, err
				}
				return agent.DialTCP(ctx, c, port)
			},
			DisableKeepAlives: true,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeError(w, http.StatusBadGateway, fmt.Errorf("cannot reach port %d in sandbox %q (is it listening?): %w", port, sb.Spec.Name, err))
		},
	}
	p.ServeHTTP(w, r)
}
