package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/coder/websocket"
)

// Listen listens on addr, which is unix:<path> or vsock:<port>.
func Listen(addr string) (net.Listener, error) {
	scheme, rest, _ := strings.Cut(addr, ":")
	switch {
	case scheme == "unix" && rest != "":
		if err := os.MkdirAll(filepath.Dir(rest), 0755); err != nil {
			return nil, err
		}
		// The lock serialises agents starting on the same path, so two of
		// them cannot both remove a stale socket and bind.
		lock, err := lockPath(rest + ".lock")
		if err != nil {
			return nil, err
		}
		if lock == nil {
			return nil, fmt.Errorf("another agent is listening on %s", rest)
		}
		if err := removeStaleSocket(rest); err != nil {
			lock.Close()
			return nil, err
		}
		l, err := net.Listen("unix", rest)
		if err != nil {
			lock.Close()
			return nil, err
		}
		return &lockedListener{l, lock}, nil
	case scheme == "vsock":
		port, err := strconv.ParseUint(rest, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("bad vsock port %q", rest)
		}
		return listenVsock(uint32(port))
	}
	return nil, fmt.Errorf("bad listen address %q: want unix:<path> or vsock:<port>", addr)
}

// lockedListener holds the lock on its path until it is closed.
type lockedListener struct {
	net.Listener
	lock *os.File
}

func (l *lockedListener) Close() error {
	err := l.Listener.Close()
	l.lock.Close() // releases the flock
	return err
}

// removeStaleSocket removes the socket at path when nothing listens on it,
// such as one left by an agent that was killed, which would make Listen
// fail. It refuses to touch a live socket or a file that is not a socket.
func removeStaleSocket(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket", path)
	}
	c, err := net.Dial("unix", path)
	if err == nil {
		c.Close()
		return fmt.Errorf("another agent is listening on %s", path)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("check %s for a running agent: %w", path, err)
	}
	return os.Remove(path)
}

// Serve serves the agent API on l until ctx is cancelled. Cancelling ctx
// also ends every exec session and kills its processes; Serve returns once
// they are gone. With shutdown nil, the agent does not serve POST
// /v1/shutdown.
func Serve(ctx context.Context, l net.Listener, version string, shutdown *Shutdown) error {
	var sessions sync.WaitGroup
	features := []string{FeatureTCP}
	mux := http.NewServeMux()
	if shutdown != nil {
		features = append(features, FeatureShutdown)
		mux.HandleFunc("POST /v1/shutdown", shutdown.serve)
	}
	mux.HandleFunc("GET /v1/files", getFiles)
	mux.HandleFunc("PUT /v1/files", putFiles)
	mux.HandleFunc("GET /v1/ports", getPorts)
	mux.HandleFunc("GET /v1/tcp", connectTCP)
	mux.HandleFunc("GET /v1/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Version{Protocol: ProtocolVersion, Agent: version, Features: features})
	})
	mux.HandleFunc("GET /v1/exec", func(w http.ResponseWriter, r *http.Request) {
		sessions.Add(1)
		defer sessions.Done()
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return // Accept has written the HTTP error
		}
		defer c.CloseNow()
		runExec(r.Context(), c)
	})
	// Request contexts derive from BaseContext, so cancelling ctx reaches
	// the hijacked exec connections too.
	srv := &http.Server{Handler: mux, BaseContext: func(net.Listener) context.Context { return ctx }}
	stop := context.AfterFunc(ctx, func() { srv.Close() })
	defer stop()
	err := srv.Serve(l)
	sessions.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// finish sends the final control message of a session and closes it. It
// reports whether the message was sent.
func finish(ctx context.Context, c *websocket.Conn, m message) bool {
	b, _ := json.Marshal(m)
	if c.Write(ctx, websocket.MessageText, b) != nil {
		return false
	}
	c.Close(websocket.StatusNormalClosure, "")
	return true
}

func execError(ctx context.Context, c *websocket.Conn, err error) {
	finish(ctx, c, message{Type: "error", Message: err.Error()})
}

func writeError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(errorResponse{Message: err.Error()})
}
