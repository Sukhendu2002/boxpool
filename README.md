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
boxpool token create --join          # on the head: a token to add a machine
boxpool join <token>  # add this machine to the head's pool
boxpool node label gpu,always-on     # labels sh --node picks machines by
boxpool sh            # open a shell in a new sandbox
boxpool sh --os macos # on the head: on a node with this OS (or --arch, --node)
boxpool exec box -- make test        # run a command in a sandbox
boxpool cp ./src box:/root/          # copy files in or out
boxpool ls            # list sandboxes; on the head, its nodes' too
boxpool ls --nodes    # on the head: the pool's machines, ready or unreachable
boxpool stop box      # stop a sandbox
boxpool start box     # start it again, with its disk
boxpool pause box     # pause a sandbox, keeping its memory
boxpool resume box    # resume it where it left off
boxpool snapshot box --name withjq   # save a copy of a sandbox's disk
boxpool sh --from withjq             # start a new sandbox from it
boxpool rm box        # delete a sandbox
boxpool doctor        # check whether this machine can host sandboxes
```

Working today, on one Linux machine: `boxpool doctor`, `boxpool daemon`,
`boxpool up`, `boxpool down`, `boxpool sh`, `boxpool exec`, `boxpool cp`,
`boxpool ls`, `boxpool start`, `boxpool stop`, `boxpool pause`, `boxpool resume`,
`boxpool rm`, `boxpool snapshot` and `boxpool version`. In a pool, `sh`,
`exec`, `cp` and sandbox lifecycle commands on the head run on the node
that owns the sandbox.

Nodes report their capacity, labels and sandboxes to the head every five
seconds over mutual TLS. After 30 seconds without a heartbeat, the head
lists the node and its sandboxes as `unreachable`, keeping their records.
The next heartbeat restores the node's actual sandbox states. Nodes open
outbound connections to the head; they need no inbound pool port.

Set labels with `boxpool up --label gpu,always-on` or replace them later
with `boxpool node label gpu,always-on`; `boxpool node label ""` clears
them. On the head, `boxpool sh --os macos`, `--arch arm64` and `--node gpu`
filter where a new sandbox can run. Among matching nodes with enough CPU
and memory after existing reservations and the host reserve (one CPU and
the larger of 1 GiB or 10% of memory), the head picks the most free memory,
then the fewest sandboxes. Existing sandbox names always return to their
owning node. Snapshots stay on their original machine; list, remove and
restore them using that machine's local CLI.

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
VMs and Docker sandboxes keep running, and the daemon adopts them when it
starts again, including after a crash.

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

While a Mac has running sandboxes, the daemon holds a `boxpool` IOKit
`PreventUserIdleSystemSleep` assertion, including during boot and guest
initialization; failed starts release it if no VM remains running.
Stopping or pausing the last one releases it. VM exits outside the API
are detected within a second.
Use `boxpool up --allow-sleep` (saved in the launch agent) or
`boxpool daemon --allow-sleep` to opt out. A later `boxpool up` without
that flag restores sleep prevention. `doctor` and `ls --nodes` warn when a
Mac is on battery; the head sees a node's power source at its next heartbeat.
This prevents **idle sleep only**, not explicit sleep or lid-close sleep.
Keep laptops plugged in and their lids open; closed-lid operation requires
an external display and macOS's other clamshell requirements.

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
the sandbox endpoints answer 503. On macOS 26 and later, sandboxes get
outbound NAT, each on a network of its own; on older macOS they have no
network. [docs/security.md](docs/security.md) covers how each backend
isolates them.

When it starts, the daemon reconciles its records with the backend. It
adopts sandboxes that are still running (on macOS none are: VMs run in the
daemon's process). A sandbox it finds stopped though it was running when
the last daemon ended, as after a host reboot, is recorded `stopped`, or
booted again if it was created with the `always` restart policy. It logs
sandboxes the backend has but its records do not, and leaves them alone.

Before reporting healthy, the VM backends prepare a private, paused base
snapshot for new sandboxes with the default 2 CPUs, 2 GiB RAM and 10 GiB
disk. Each restore gets a private disk, hostname, machine ID and network
address; the guest RNG is reseeded. The cache is rebuilt on image upgrades.
Custom sizes, `--from` snapshots and unsupported hosts keep cold-booting;
cache preparation failures log a warning rather than prevent startup.
See [warm-boot testing](docs/benchmarks/m3-warm-boot.md).

`boxpool sh [--name N] [--cpu N] [--mem 2g] [--disk 10g] [--restart always] [--timing]`
asks the daemon for a sandbox (a new one with a generated name, or the one
called `N`), starts it if needed and opens `bash -l` in it on your terminal.
`--restart always` (`"restart": "always"` in the API) boots a new sandbox
again when the daemon starts after a host reboot.
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

`boxpool ls [--json]` lists local sandboxes followed by the head's nodes'
sandboxes, with name, backend, host OS, state, CPUs, memory and age, plus a
node column when remote sandboxes exist. Local state is live (`missing` if
the backend no longer has the sandbox); remote state comes from heartbeats.
An unreachable node's last-known sandboxes stay listed as `unreachable`.
`boxpool ls --nodes [--json]` shows node health, labels and free CPU/memory
capacity bars. Bars show free admission capacity against totals after the
host reserve, not instantaneous CPU utilization; stopped sandboxes keep
their reservations. Unreachable rows are last-known values; `?` means an
older node did not report totals.

`boxpool logs [-f] <sandbox>` reads the current sandbox console log, routed
to its node. `boxpool logs [-f] --daemon` reads this node's daemon log.
`-f` follows output until Ctrl-C or daemon shutdown. Followers do not delay
a clean daemon shutdown. Flags precede the sandbox name. VM logs
are serial-console output, not shell/exec session output; send an entry
with `boxpool exec <sandbox> -- sh -c 'echo hello > /dev/console'`.
Docker logs are the container's main process stdout/stderr, likewise not
exec output. VM console and daemon files rotate at about 1 MiB, retaining
one previous file; `logs` reads only the current file. Following handles
renames and observed truncations, but copy-truncate rotation may lose
output between polls; this is not a durable log delivery service.
Daemon logs are also saved as `daemon.log` in the state directory, including
foreground runs. The API is `GET /v1/sandboxes/{name}/logs` or `GET /v1/logs`,
with `?follow=true` to stream.

`boxpool stop <name>` shuts a sandbox down and keeps its disk and its
resource reservation. `boxpool start <name>` boots a stopped sandbox from
that disk, including after a daemon or host restart; on Docker only `/root`
is kept. `boxpool rm [--force] <name>` deletes a stopped
sandbox with its process, disk, network and reservation; `--force` stops a
running one first. If a guest does not shut down cleanly, both print a
warning and still succeed. The API is `GET /v1/sandboxes`,
`POST /v1/sandboxes/{name}/start`, `POST /v1/sandboxes/{name}/stop` and `DELETE /v1/sandboxes/{name}?force=1`.

`boxpool pause <name>` saves a running sandbox's memory and stops its VM,
so it uses no CPU or memory; `boxpool ls` shows it `paused`, and
`boxpool resume <name>` restores it where it left off. Docker cannot save
a container's memory, so there pause stops the sandbox, prints that its
memory is lost, and resume boots it from its disk; so does a macOS VM
configuration the Virtualization framework cannot save. The API is
`POST /v1/sandboxes/{name}/pause` and `POST /v1/sandboxes/{name}/resume`.

`boxpool snapshot <name> --name <snapshot>` saves a copy of a sandbox's
disk, and `boxpool sh --from <snapshot>` starts new sandboxes from it on
the same machine, each with its own copy. A running sandbox is stopped for
the copy, so the disk is consistent, then started again; its processes
restart. A paused one is refused. On Firecracker and macOS the whole disk
is copied, installed packages too, as a reflink or APFS clone where the
file system can; on Docker only `/root` persists, so only `/root` is saved.
`boxpool snapshot ls` lists snapshots and `boxpool snapshot rm <snapshot>`
deletes one; sandboxes started from it keep their disks. `boxpool sh ... --
<command>` runs a command instead of the shell. The API is
`POST /v1/sandboxes/{name}/snapshot` (`{"name": "..."}`), `GET /v1/snapshots`,
`DELETE /v1/snapshots/{name}`, and `"from"` in `POST /v1/sandboxes`.

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
