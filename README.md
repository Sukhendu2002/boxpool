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

Only `boxpool doctor` and `boxpool version` work today.

## Supported hosts (v1)

- Linux, x86_64 or aarch64, with `/dev/kvm` for microVMs (Docker fallback without it)
- macOS 14 or later on Apple silicon

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
