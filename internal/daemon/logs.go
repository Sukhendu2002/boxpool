package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"boxpool.run/boxpool/internal/backend"
)

const LogName = "daemon.log"

// OpenLog keeps a bounded daemon log for both service and foreground runs.
func OpenLog(dir string) (io.WriteCloser, error) {
	if err := prepareStateDir(dir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, LogName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	return &logWriter{File: f}, nil
}

type logWriter struct {
	*os.File
	mu sync.Mutex
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := backend.RotateConsole(w.Name()); err != nil {
		return 0, err
	}
	return w.File.Write(p)
}

// logSession also wraps routing, so remote followers cannot delay head shutdown.
func (s *server) logSession(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		follow, _ := strconv.ParseBool(r.URL.Query().Get("follow"))
		if !follow {
			h(w, r)
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		stop := context.AfterFunc(s.logShutdown, cancel)
		defer stop()
		defer cancel()
		h(w, r.WithContext(ctx))
	}
}

func (s *server) daemonLogs(w http.ResponseWriter, r *http.Request) {
	follow, ok := logFollow(w, r)
	if !ok {
		return
	}
	logs, err := backend.OpenConsole(r.Context(), filepath.Join(s.info.StateDir, LogName), follow)
	streamLogs(w, r, logs, err)
}

func (s *server) sandboxLogs(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if err := s.sameBackend(sb); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	follow, ok := logFollow(w, r)
	if !ok {
		return
	}
	d, ok := s.d.(interface {
		Logs(context.Context, string, bool) (io.ReadCloser, error)
	})
	if !ok {
		writeError(w, http.StatusNotImplemented, backend.ErrNotSupported)
		return
	}
	logs, err := d.Logs(r.Context(), sb.ID, follow)
	streamLogs(w, r, logs, err)
}

func logFollow(w http.ResponseWriter, r *http.Request) (bool, bool) {
	value := r.URL.Query().Get("follow")
	if value == "" {
		return false, true
	}
	follow, err := strconv.ParseBool(value)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return false, false
	}
	return follow, true
}

func streamLogs(w http.ResponseWriter, r *http.Request, logs io.ReadCloser, err error) {
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}
	defer logs.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if err := http.NewResponseController(w).Flush(); err != nil {
		return
	}
	if _, err := io.Copy(logFlusher{w}, logs); err != nil && r.Context().Err() == nil {
		slog.Warn("streaming logs failed", "err", err)
		panic(http.ErrAbortHandler) // never report a failed stream as complete
	}
}

type logFlusher struct{ http.ResponseWriter }

func (w logFlusher) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if err == nil {
		err = http.NewResponseController(w.ResponseWriter).Flush()
	}
	return n, err
}
