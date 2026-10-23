# Boxpool

Turn your spare machines into a pool of sandboxes for AI agents.

Boxpool pools the Linux boxes and Apple silicon Macs you already own into
sandboxes your agents can run in. Each sandbox is its own Linux VM
(Firecracker on Linux, Apple's Virtualization framework on macOS), reachable
from anywhere, with no per-minute bills.

> Status: early development. Nothing here is usable yet.

## Planned commands

```sh
boxpool up            # start a pool on this machine (head node)
boxpool join <token>  # add another machine to the pool
boxpool sh            # open a shell in a new sandbox
boxpool ls            # list nodes and sandboxes
boxpool doctor        # check whether this machine can host sandboxes
```

Only `boxpool doctor`, `boxpool daemon`, `boxpool sh`, `boxpool ls`,
`boxpool stop`, `boxpool rm` and `boxpool version` work today, on one Linux
machine.

`boxpool daemon` runs the node daemon in the foreground. It creates the state
directory (below), takes `daemon.lock` there so only one daemon runs per state
directory, and serves a local HTTP API on `boxpool.sock` (mode 0660) until
SIGINT or SIGTERM. It refuses a state directory that group or others can
write to:

```sh
BOXPOOL_HOME=$(mktemp -d) ./bin/boxpool daemon &
curl --unix-socket "$BOXPOOL_HOME/boxpool.sock" http://x/v1/health   # {"ok":true}
curl --unix-socket "$BOXPOOL_HOME/boxpool.sock" http://x/v1/info
```

On Linux the daemon opens the backend doctor picks: Firecracker, after
downloading its pinned artifacts into `artifacts/` in the state directory
(about 60 MB the first time), or Docker with a warning. Firecracker
sandboxes get a network only when the daemon runs as root. With no backend,
the sandbox endpoints answer 503.

`boxpool sh [--name N] [--cpu N] [--mem 2g] [--disk 10g] [--timing]` asks the
daemon for a sandbox (a new one with a generated name, or the one called
`N`), starts it if needed and opens `bash -l` in it on your terminal.
`boxpool sh <name>` attaches to an existing sandbox. Leaving the shell
(`exit`, Ctrl-D) leaves the sandbox running; `--timing` prints the time to
the first prompt. The daemon's API for this is `POST /v1/sandboxes`
(idempotent on `name`), `GET /v1/sandboxes/{name}` and the WebSocket
`GET /v1/sandboxes/{name}/exec`, which relays the agent's exec protocol
unchanged.

`boxpool version` also prints the daemon's version when one is running.

`boxpool ls [--json]` lists this machine's sandboxes, oldest first, with
their name, backend, state, CPUs, memory and age; the state is the
backend's live one (`missing` if the backend no longer has the sandbox).
`boxpool stop <name>` shuts a sandbox down and keeps its disk and its
resource reservation. `boxpool rm [--force] <name>` deletes a stopped
sandbox with its process, disk, network and reservation; `--force` stops a
running one first. If a guest does not shut down cleanly, both print a
warning and still succeed. The API is `GET /v1/sandboxes`,
`POST /v1/sandboxes/{name}/stop` and `DELETE /v1/sandboxes/{name}?force=1`.

`boxpool-agent` is the process inside every sandbox that runs commands for
the host, on vsock port 1024 by default or on a unix socket with
`--listen unix:<path>`. Its API is in the
[guest agent protocol](docs/agent-protocol.md). Build it as a static Linux
binary with `CGO_ENABLED=0 GOOS=linux go build -o bin/boxpool-agent ./cmd/boxpool-agent`.

## Supported hosts (v1)

- Linux 5.10 or newer, x86_64 or aarch64, with `/dev/kvm` for microVMs (Docker fallback without it)
- macOS 14 or later on Apple silicon

`boxpool doctor` requires at least 20 GB of free disk space in the state
directory: `BOXPOOL_HOME` when set, otherwise `/var/lib/boxpool` on Linux or
`~/Library/Application Support/boxpool` on macOS. If it does not exist yet,
doctor measures the nearest existing parent and reports that path. A disk or
kernel failure exits non-zero even if a backend is available. On Linux, `!`
marks a VM warning: nested virtualization may be unavailable or slow, but
the warning does not affect the backend or exit status.

## Development

See the [Linux and KVM development setup](docs/development.md) for a Linux
machine or VM, including how to build on a Mac and run there.

```sh
go build -o bin/boxpool ./cmd/boxpool
go vet ./...
go test ./...
./bin/boxpool doctor
```

## License

Apache 2.0. See [LICENSE](LICENSE).
