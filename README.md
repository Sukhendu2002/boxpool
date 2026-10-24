# Boxpool

Turn your spare machines into a pool of sandboxes for AI agents.

Boxpool pools the Linux boxes and Apple silicon Macs you already own into
sandboxes your agents can run in. Each sandbox is its own Linux VM
(Firecracker on Linux, Apple's Virtualization framework on macOS), reachable
from anywhere, with no per-minute bills.

> Status: early development. Nothing here is usable yet.

## Install from source (Linux)

Use a Linux 5.10+ host with systemd, at least 20 GB free in `/var/lib`,
Git and Go 1.24+. Firecracker needs `/dev/kvm`, `e2fsprogs` (for
`resize2fs`), `ip` (the `iproute2` package on Debian/Ubuntu, `iproute` on
Fedora), and `nft` (`nftables`) for root-daemon networking. Without KVM,
install and start Docker Engine for the fallback.
See the [Linux setup](docs/development.md) for distribution package commands.

```sh
git clone https://github.com/Sukhendu2002/boxpool.git
cd boxpool
go build -o bin/boxpool ./cmd/boxpool
sudo ./bin/boxpool up
sudo /usr/local/bin/boxpool sh
```

`up` installs the binary at `/usr/local/bin/boxpool` and starts the root
daemon. If a preflight check fails, fix it and rerun `sudo ./bin/boxpool up`.
The first start needs internet access to download Firecracker and the guest
image. Exit the sandbox shell with `exit`; the sandbox stays running.

**After `up`, run client commands with `sudo`**, for example
`sudo boxpool ls`, `sudo boxpool exec <name> -- uname -a`,
`sudo boxpool cp ./src <name>:/root/`, and `sudo boxpool sh <name>`.
Use `/usr/local/bin/boxpool` if it is not on sudo's `PATH`. Leave
`BOXPOOL_HOME` unset to use the service's `/var/lib/boxpool` state directory.
Files copied to the host with `sudo boxpool cp` are owned by root.
Stop the service with `sudo boxpool down`.

The service's socket, `/var/lib/boxpool/boxpool.sock`, is `root:root` mode
`0660`, inside a state directory created as `root:root` mode `0750`.
A normal user cannot search that directory, so **permission denied does
not tell you whether the daemon is running**: it can also happen after
`sudo boxpool down` removes the socket. Retry with
`sudo /usr/local/bin/boxpool ls` to check with the required access. If the
socket is missing or refuses connections, the client gives a startup hint.
Do not start another daemon just to fix permissions or loosen directory
permissions to check its status.

No `boxpool` group is created: access to a root daemon's socket is
effectively root access to the host (it can create VMs and copy files as
root), so do not make it
world-accessible or share it with untrusted users. A foreground daemon
started without sudo is instead used by the same user, with the same
`BOXPOOL_HOME` if set.

## Commands

```sh
boxpool up            # install and start the daemon on this machine
boxpool down          # stop the daemon, keeping its state
boxpool join <token>  # add another machine to the pool (planned)
boxpool sh            # open a shell in a new sandbox
boxpool exec box -- make test        # run a command in a sandbox
boxpool cp ./src box:/root/          # copy files in or out
boxpool ls            # list sandboxes (nodes too, once pools exist)
boxpool stop box      # stop a sandbox
boxpool rm box        # delete a sandbox
boxpool doctor        # check whether this machine can host sandboxes
```

Working today, on one Linux machine: `boxpool doctor`, `boxpool daemon`,
`boxpool up`, `boxpool down`, `boxpool sh`, `boxpool exec`, `boxpool cp`,
`boxpool ls`, `boxpool stop`, `boxpool rm` and `boxpool version`. `join` is
not implemented yet.

`sudo boxpool up` installs the node on a Linux machine with systemd: it
runs doctor's checks and refuses if they fail, copies the running binary
to `/usr/local/bin/boxpool`, writes `/etc/systemd/system/boxpool.service`
(`boxpool daemon` as root, restarted on failure), enables and starts it
and waits until the daemon answers on `/var/lib/boxpool/boxpool.sock`. The
first start downloads Firecracker and the guest image, about 60 MB.
Running it again changes nothing unless the binary or the unit changed, in
which case it restarts the service. `sudo boxpool down` stops and disables
the service and keeps `/var/lib/boxpool`. Stopping or restarting the
service stops only the daemon (`KillMode=process`): running Firecracker
VMs keep running until the daemon starts again, which today kills them
(#27 will re-adopt them); Docker sandboxes keep running.

On macOS, run `boxpool up` as yourself, not with sudo: the daemon runs as
your user. Build and sign it first (see
[macOS on Apple silicon](docs/development.md#macos-on-apple-silicon)); `up`
refuses a binary without the virtualization entitlement. It runs doctor's
checks, copies the binary to `bin/boxpool` in the state directory, writes
the launch agent `~/Library/LaunchAgents/run.boxpool.daemon.plist`
(restarted if it fails, logging to `~/Library/Logs/boxpool/daemon.log`),
starts it in your login session and waits until the daemon answers. A
`BOXPOOL_HOME` set when you run `up` is written into the agent. launchd
starts the agent again at every login. Running `up` again changes nothing
unless the binary or the plist changed, in which case it restarts the
daemon. `boxpool down` stops the agent, which stops its VMs, and removes
the plist; the state directory and the installed binary are kept.

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

`boxpool exec [-i] [-t] <sandbox> [--] <command...>` runs a command in a
running sandbox and exits with its exit code. Without `-t` its stdout and
stderr stay separate and its stdin is empty, unless `-i` forwards yours;
`-t` runs it on your terminal in raw mode, as `sh` does. As with
`docker exec`, it exits 125 when `boxpool exec` itself fails (bad
arguments, no daemon, sandbox missing or stopped, connection lost before
the command exited), else with the command's code, or 128+n if signal n
killed it; a command that exits 125 itself looks the same.

`boxpool cp SRC DST` copies a file or directory between this machine and a
sandbox; exactly one side is `<sandbox>:<absolute path>`. Like `docker cp`,
the copy keeps modes, modification times and symlinks (not owners): into
the sandbox it lands inside the existing directory DST; out of it, inside
DST if that is a directory, else as DST. Copies out of a sandbox are
treated as hostile: every entry must be inside the copy you asked for (no
absolute paths, `..`, or writes through symlinks), hard links and devices
are refused, and set-user-ID and set-group-ID bits are dropped. The API is
`GET` and `PUT /v1/sandboxes/{name}/files?path=`, which relay the agent's
tar endpoints unchanged.

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
