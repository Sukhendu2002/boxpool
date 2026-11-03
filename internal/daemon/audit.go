package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"boxpool.run/boxpool/internal/auth"
	"boxpool.run/boxpool/internal/store"
)

type auditActorKey struct{}

func auditActor(r *http.Request) string {
	if id, ok := r.Context().Value(auditActorKey{}).(string); ok {
		return id
	}
	return "local"
}

func (s *server) recordAudit(r *http.Request, action, target string, extra ...store.AuditEvent) error {
	e := store.AuditEvent{Time: time.Now().UTC(), TokenID: auditActor(r), Action: action, Target: target}
	for i := range extra {
		extra[i].Time, extra[i].TokenID = e.Time, e.TokenID
	}
	// A client disconnect must not erase an action that already happened.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	err := s.st.AppendAudit(ctx, append(extra, e)...)
	if err != nil {
		slog.Error("cannot record audit event", "action", action, "target", target, "err", err)
	}
	return err
}

// audit records successful operations on the head, outside placement and
// routing so that forwarded actions retain the original caller's identity.
// Mutation responses are small JSON records: hold them until audit commits.
func (s *server) audit(action string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isHead() {
			h(w, r)
			return
		}
		if action == "sandbox.exec" {
			h(&auditUpgradeWriter{ResponseWriter: w, record: func() error {
				return s.recordAudit(r, action, r.PathValue("name"))
			}}, r)
			return
		}
		out := &auditResponse{header: make(http.Header)}
		h(out, r)
		out.WriteHeader(http.StatusOK)
		if out.status >= 200 && out.status < 300 {
			target, extra, err := auditTarget(action, r, out)
			if err == nil && target != "" {
				err = s.recordAudit(r, action, target, extra...)
			}
			if err != nil {
				writeError(w, http.StatusInternalServerError, fmt.Errorf("operation completed but audit failed: %w", err))
				return
			}
		}
		for k, v := range out.header {
			w.Header()[k] = v
		}
		w.WriteHeader(out.status)
		w.Write(out.body.Bytes())
	}
}

type auditResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
	extra  []store.AuditEvent
}

func (w *auditResponse) Header() http.Header { return w.header }
func (w *auditResponse) WriteHeader(status int) {
	// A node may send 100 Continue before its final mutation response.
	// Upgrades use auditUpgradeWriter rather than this buffered writer.
	if status >= 100 && status < 200 {
		return
	}
	if w.status == 0 {
		w.status = status
	}
}
func (w *auditResponse) Write(b []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.body.Write(b)
}

// addAuditEvent reports an actual side effect for the middleware to append
// only when the enclosing operation succeeds, without changing its response.
func addAuditEvent(w http.ResponseWriter, action, target string) {
	if out, ok := w.(*auditResponse); ok {
		out.extra = append(out.extra, store.AuditEvent{Action: action, Target: target})
	}
}

// auditTarget reads only server response identities, never request bodies,
// command arguments, cookies or URL queries, which may contain secrets.
func auditTarget(action string, r *http.Request, out *auditResponse) (string, []store.AuditEvent, error) {
	switch action {
	case "sandbox.create":
		if out.status != http.StatusCreated { // sh also starts existing sandboxes
			return "", nil, nil
		}
		var sb Sandbox
		err := json.Unmarshal(out.body.Bytes(), &sb)
		return sb.Name, nil, err
	case "sandbox.rm":
		return r.PathValue("name"), nil, nil
	case "sandbox.expose", "sandbox.unexpose":
		var e Expose
		if err := json.Unmarshal(out.body.Bytes(), &e); err != nil {
			return "", nil, err
		}
		extra := out.extra
		if e.TokenID != "" && action == "sandbox.expose" {
			extra = append(extra, store.AuditEvent{Action: "token.create", Target: e.TokenID})
		}
		return net.JoinHostPort(e.Sandbox, strconv.Itoa(e.Port)), extra, nil
	case "token.create":
		var tok AccessToken
		if err := json.Unmarshal(out.body.Bytes(), &tok); err != nil {
			return "", nil, err
		}
		if tok.ID != "" {
			return tok.ID, nil, nil
		}
		join, err := auth.ParseJoinToken(tok.Token)
		if err != nil {
			return "", nil, err
		}
		return join.Hash()[:store.AccessIDLen], nil, nil
	case "token.revoke":
		if id := r.PathValue("id"); id != "" {
			return id, nil, nil
		}
		hash := r.PathValue("hash")
		return hash[:min(len(hash), store.AccessIDLen)], nil, nil
	case "login":
		return auditActor(r), nil, nil
	}
	return "", nil, fmt.Errorf("unknown audit action %q", action)
}

// WebSocket handshakes hijack HTTP/1 connections without WriteHeader.
// Record at the successful upgrade, before the session can run commands,
// rather than waiting for a potentially long-lived shell to close.
type auditUpgradeWriter struct {
	http.ResponseWriter
	record func() error
}

func (w *auditUpgradeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *auditUpgradeWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	if err := w.record(); err != nil {
		c.Close()
		return nil, nil, err
	}
	return c, rw, nil
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	if auditActor(r) == "local" {
		writeError(w, http.StatusUnauthorized, errors.New("login requires an access token"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) listAudit(w http.ResponseWriter, r *http.Request) {
	if !s.isHead() {
		writeError(w, http.StatusConflict, errors.New("read the audit log on the head"))
		return
	}
	limit, before := 100, int64(0)
	var err error
	if q := r.URL.Query().Get("limit"); q != "" {
		limit, err = strconv.Atoi(q)
	}
	if q := r.URL.Query().Get("before"); q != "" && err == nil {
		before, err = strconv.ParseInt(q, 10, 64)
	}
	if err != nil || limit < 1 || limit > 1000 || before < 0 {
		writeError(w, http.StatusBadRequest, errors.New("before must be nonnegative and limit must be between 1 and 1000"))
		return
	}
	list, err := s.st.ListAudit(r.Context(), before, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, list)
}
