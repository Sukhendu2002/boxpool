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

Only `boxpool doctor`, `boxpool daemon` and `boxpool version` work today.

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

`boxpool version` also prints the daemon's version when one is running.

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
