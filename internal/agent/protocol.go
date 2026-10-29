// Package agent is boxpool-agent, the process inside every sandbox that runs
// commands for the host, and a client for it. docs/agent-protocol.md
// describes the wire protocol.
package agent

// ProtocolVersion is the agent protocol version reported by GET /v1/version.
// It changes only when a change would break existing clients.
const ProtocolVersion = 1

// Stream ids: the first byte of every binary WebSocket message.
const (
	Stdin  byte = 0
	Stdout byte = 1
	Stderr byte = 2
)

// maxMessage is the largest WebSocket message either side sends. It is
// coder/websocket's default read limit, so no client needs to raise it.
const maxMessage = 32768

// ExecRequest is the first message of an exec session.
type ExecRequest struct {
	Argv []string `json:"argv"`
	Env  []string `json:"env,omitempty"`
	Cwd  string   `json:"cwd,omitempty"`
	TTY  bool     `json:"tty,omitempty"`
	Rows uint16   `json:"rows,omitempty"`
	Cols uint16   `json:"cols,omitempty"`
}

// Version is the body of GET /v1/version.
type Version struct {
	Protocol int      `json:"protocol"`
	Agent    string   `json:"agent"`
	Features []string `json:"features,omitempty"`
}

// FeatureShutdown in Version.Features means the agent serves POST
// /v1/shutdown. Agents older than it, and those in containers, lack it.
const FeatureShutdown = "shutdown"

// ShutdownRequest is the body of POST /v1/shutdown.
type ShutdownRequest struct {
	GraceMS int64 `json:"grace_ms,omitempty"`
}

// message is a JSON control message; Type says which other fields are set.
type message struct {
	Type    string `json:"type"`
	Rows    uint16 `json:"rows,omitempty"`
	Cols    uint16 `json:"cols,omitempty"`
	Code    *int   `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// Port is a listening TCP socket reported by GET /v1/ports.
type Port struct {
	Proto   string `json:"proto"`
	Address string `json:"address"`
	Port    int    `json:"port"`
}

type errorResponse struct {
	Message string `json:"message"`
}
