package agent

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

// FeatureDesktop advertises an image with an on-demand virtual desktop.
const FeatureDesktop = "desktop"

//go:embed desktopassets
var desktopAssets embed.FS

// Desktop describes the display and its private RFB endpoint. HumanControl is
// cooperative: agents should pause their browser automation while it is true.
type Desktop struct {
	Display      string `json:"display"`
	VNC          string `json:"vnc"`
	HumanControl bool   `json:"human_control"`
}

// One viewer owns the input policy. Disconnecting always returns to watching.
// The Unix socket is never published on the guest network.
type desktop struct {
	ctx     context.Context
	mu      sync.Mutex
	done    chan struct{}
	stop    context.CancelFunc
	viewer  bool
	human   bool
	command string
	socket  string
	input   func(context.Context, bool) error
}

func newDesktop(ctx context.Context) *desktop {
	return &desktop{ctx: ctx, command: "boxpool-desktop", socket: "/run/boxpool/desktop.vnc", input: desktopInput}
}

func desktopInput(ctx context.Context, human bool) error {
	value := "0"
	if human {
		value = "1"
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tigervncconfig", "-display", ":0", "-set", "AcceptKeyEvents="+value, "AcceptPointerEvents="+value).CombinedOutput()
	if err != nil {
		return fmt.Errorf("set desktop input: %w: %s", err, out)
	}
	return nil
}

func (d *desktop) close() {
	d.mu.Lock()
	stop, done := d.stop, d.done
	d.mu.Unlock()
	if stop != nil && done != nil {
		stop()
		<-done
	}
}

// start is idempotent, and a failed/crashed session can be started again.
func (d *desktop) start(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.done != nil {
		select {
		case <-d.done:
			if d.stop != nil {
				d.stop()
			}
			d.stop, d.done = nil, nil
		default:
			d.describe(w)
			return
		}
	}
	path, err := exec.LookPath(d.command)
	if err != nil {
		writeError(w, http.StatusConflict, errors.New("this image has no desktop; create a sandbox with --image desktop"))
		return
	}
	ctx, cancel := context.WithCancel(d.ctx)
	cmd := exec.CommandContext(ctx, path)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 3 * time.Second
	if err := cmd.Start(); err != nil {
		cancel()
		writeError(w, http.StatusBadGateway, err)
		return
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	// Startup is bounded by the request, but the desktop belongs to Serve.
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		conn, err := net.DialTimeout("unix", d.socket, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			d.done, d.stop, d.human = done, cancel, false
			d.describe(w)
			return
		}
		select {
		case <-done:
			err = errors.New("desktop exited during startup; check the agent log")
		case <-timer.C:
			err = errors.New("desktop startup timed out")
		case <-r.Context().Done():
			err = r.Context().Err()
		case <-tick.C:
			continue
		}
		cancel()
		<-done
		writeError(w, http.StatusBadGateway, err)
		return
	}
}

func (d *desktop) describe(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(Desktop{Display: ":0", VNC: "unix:" + d.socket, HumanControl: d.human})
}

func (d *desktop) status(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.describe(w)
}

func (d *desktop) control(w http.ResponseWriter, r *http.Request) {
	// Fetches must be same-origin. The head validates the external Origin before
	// translating it to the agent host; arbitrary origins remain unchanged.
	if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
		http.Error(w, "foreign origin", http.StatusForbidden)
		return
	}
	var req struct {
		Human bool `json:"human_control"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.viewer {
		writeError(w, http.StatusConflict, errors.New("connect the viewer first"))
		return
	}
	if err := d.input(r.Context(), req.Human); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	d.human = req.Human
	d.describe(w)
}

func (d *desktop) stream(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	if d.viewer {
		d.mu.Unlock()
		http.Error(w, "a viewer is already connected", http.StatusConflict)
		return
	}
	up, err := net.DialTimeout("unix", d.socket, time.Second)
	if err != nil {
		d.mu.Unlock()
		writeError(w, http.StatusConflict, errors.New("start the desktop first"))
		return
	}
	// Reset the VNC server before every viewer, even after a previous failure.
	if err := d.input(r.Context(), false); err != nil {
		up.Close()
		d.mu.Unlock()
		writeError(w, http.StatusBadGateway, err)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		up.Close()
		d.mu.Unlock()
		return
	}
	d.viewer, d.human = true, false
	d.mu.Unlock()
	defer func() {
		up.Close()
		c.CloseNow()
		d.mu.Lock()
		defer d.mu.Unlock()
		if err := d.input(d.ctx, false); err != nil {
			// Fail closed if resetting input fails: restart the desktop next time.
			if d.stop != nil {
				d.stop()
			}
		}
		d.viewer, d.human = false, false
	}()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(ctx, func() { up.Close(); c.CloseNow() })
	defer stop()
	// NetConn treats binary messages as a byte stream (the noVNC websockify
	// protocol) and preserves fragmentation.
	down := websocket.NetConn(ctx, c, websocket.MessageBinary)
	done := make(chan struct{})
	go func() { io.Copy(up, down); cancel(); close(done) }()
	io.Copy(down, up)
	cancel()
	<-done
}

func (d *desktop) routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/desktop", d.start)
	mux.HandleFunc("GET /v1/desktop", d.status)
	mux.HandleFunc("POST /v1/desktop/control", d.control)
	mux.HandleFunc("GET /v1/desktop/ws", d.stream)
	mux.Handle("GET /v1/desktop/", DesktopViewer())
}

// DesktopViewer serves the embedded noVNC client below /v1/desktop/.
// Both the host binary and guest agent embed it; no CDN is required.
func DesktopViewer() http.Handler {
	assets, _ := fs.Sub(desktopAssets, "desktopassets")
	files := http.StripPrefix("/v1/desktop/", http.FileServer(http.FS(assets)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data: blob:; frame-ancestors 'none'")
		// FileServer's index redirect must not produce a path outside the expose.
		if strings.Contains(r.URL.Path, "..") {
			http.NotFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	})
}
