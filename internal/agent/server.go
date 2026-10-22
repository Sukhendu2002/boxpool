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
		// A socket left by an agent that was killed would make Listen fail.
		if fi, err := os.Lstat(rest); err == nil && fi.Mode()&os.ModeSocket != 0 {
			os.Remove(rest)
		}
		return net.Listen("unix", rest)
	case scheme == "vsock":
		port, err := strconv.ParseUint(rest, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("bad vsock port %q", rest)
		}
		return listenVsock(uint32(port))
	}
	return nil, fmt.Errorf("bad listen address %q: want unix:<path> or vsock:<port>", addr)
}

// Serve serves the agent API on l until ctx is cancelled. Cancelling ctx
// also ends every exec session and kills its processes; Serve returns once
// they are gone.
func Serve(ctx context.Context, l net.Listener, version string) error {
	var sessions sync.WaitGroup
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Version{Protocol: ProtocolVersion, Agent: version})
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

// finish sends the final control message of a session and closes it.
func finish(ctx context.Context, c *websocket.Conn, m message) {
	b, _ := json.Marshal(m)
	if c.Write(ctx, websocket.MessageText, b) == nil {
		c.Close(websocket.StatusNormalClosure, "")
	}
}

func execError(ctx context.Context, c *websocket.Conn, err error) {
	finish(ctx, c, message{Type: "error", Message: err.Error()})
}
