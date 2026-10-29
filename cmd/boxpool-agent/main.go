// Command boxpool-agent runs inside every sandbox and executes commands for
// the host. docs/agent-protocol.md describes its API.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"boxpool.run/boxpool/internal/agent"
)

var version = "0.0.0-dev"

func main() {
	listen := flag.String("listen", "vsock:1024", "address to listen on: vsock:<port> or unix:<path>")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	l, err := agent.Listen(*listen)
	if err != nil {
		slog.Error("could not listen", "addr", *listen, "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.Info("agent started", "listen", *listen, "version", version)
	// Only a VM's agent, on vsock, may stop every process and reboot.
	var shutdown *agent.Shutdown
	if strings.HasPrefix(*listen, "vsock:") {
		shutdown = agent.GuestShutdown()
	}
	if err := agent.Serve(ctx, l, version, shutdown); err != nil {
		slog.Error("agent failed", "err", err)
		os.Exit(1)
	}
	slog.Info("agent stopped")
}
