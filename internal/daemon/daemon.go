// Package daemon is the node daemon: it owns this machine's sandboxes and
// serves the local HTTP API on a unix socket in the state directory.
package daemon

import (
	"encoding/json"
	"net/http"
)

// SocketName is the API socket's file name inside the state directory.
const SocketName = "boxpool.sock"

// StoreName is the sandbox database's file name inside the state directory.
const StoreName = "state.db"

// Info describes the daemon and the node it runs on.
type Info struct {
	Version  string `json:"version"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	StateDir string `json:"state_dir"`
	Backend  string `json:"backend"`
	// Warning, if set, is about the backend, e.g. that it is a fallback with
	// weaker isolation. Commands that create sandboxes print it.
	Warning string `json:"warning,omitempty"`
}

// Handler returns the daemon's HTTP API.
func Handler(info Info) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, info)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
