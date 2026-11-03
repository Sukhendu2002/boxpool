package daemon

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"

	"boxpool.run/boxpool/internal/agent"
)

// DesktopPort reserves the non-TCP port 0 in the expose store for the agent's
// desktop service. It cannot collide with a sandbox web application's port.
const DesktopPort = 0

// desktop proxies only the desktop API, over the backend's agent channel.
// route forwards it through the same node tunnels on Linux and macOS.
func (s *server) desktop(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if err := s.sameBackend(sb); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	prefix := "/v1/sandboxes/" + sb.Spec.Name + "/desktop"
	rest, ok := strings.CutPrefix(r.URL.Path, prefix)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet && rest != "" && rest != "/ws" {
		r.URL.Path, r.URL.RawPath = "/v1/desktop"+rest, ""
		agent.DesktopViewer().ServeHTTP(w, r)
		return
	}
	p := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.Host = "http", "agent", "agent"
			pr.Out.URL.Path, pr.Out.URL.RawPath = "/v1/desktop"+rest, ""
		},
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return s.d.Dial(ctx, sb.ID) }, DisableKeepAlives: true},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeError(w, http.StatusBadGateway, fmt.Errorf("desktop in sandbox %q: %w", sb.Spec.Name, err))
		},
	}
	p.ServeHTTP(w, r)
}
