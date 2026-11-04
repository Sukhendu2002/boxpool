# User guide

Start with the [two-machine quickstart](../README.md#quickstart-a-two-machine-pool).
Examples here use `boxpool`; on Linux with the installed root daemon, use
`sudo /usr/local/bin/boxpool` instead. Put flags before positional arguments
unless the command help explicitly shows otherwise. `exec` flags go before
the sandbox name; flags after `--` belong to the guest command.

## Installation options

The [installer](https://boxpool.run/install) supports Linux amd64/arm64
and Apple silicon macOS. It chooses `/usr/local/bin` when writable,
otherwise `~/.local/bin`, without invoking sudo. To choose a directory or
pin a release:

```sh
curl -fsSL https://boxpool.run/install -o /tmp/boxpool-install.sh
BOXPOOL_INSTALL_DIR="$HOME/.local/bin" BOXPOOL_VERSION=v0.1.1 sh /tmp/boxpool-install.sh
export PATH="$HOME/.local/bin:$PATH"
boxpool version
```

Homebrew: `brew install sukhendu2002/tap/boxpool`.
Node.js 20+: `npm install -g boxpool`, or `npx boxpool@0.1.1 version` to
run a pinned version without install hooks. npm downloads on first use;
its launcher verifies the pinned digest even for cached binaries.
`BOXPOOL_CACHE_DIR` overrides its download cache. Homebrew pins SHA-256,
and the shell installer checks the release manifest. These checks detect
corruption; they are not an independent signature if the release account
is compromised. macOS releases are ad-hoc signed, not notarized; see
[verification](development.md#releases).

To build from source, install Git and Go 1.25+, plus the host prerequisites
in the quickstart. On macOS, install Xcode Command Line Tools first
(`xcode-select --install`); sandbox networking also needs the macOS 26 SDK.

```sh
git clone https://github.com/Sukhendu2002/boxpool.git
cd boxpool
go build -o bin/boxpool ./cmd/boxpool
# macOS only: sign with the virtualization entitlement
scripts/dev-sign.sh bin/boxpool
```

On Linux, skip the signing step and run `sudo ./bin/boxpool up`.
On a Mac, run `./bin/boxpool up` as yourself.
Continue with the [quickstart](../README.md#2-start-the-head).

## Shells, commands and files

```sh
boxpool sh --name work --cpu 1 --mem 512m --disk 10g
# Run inside the guest: exit
boxpool sh work
# Run inside the guest: exit
boxpool exec work -- uname -a
boxpool exec -i work -- sh -c 'cat > /root/input.txt' < ./input.txt
boxpool cp ./src work:/root/
boxpool cp work:/root/src ./downloaded-src
```

`sh --name work` creates a sandbox if needed; `sh work` attaches to an
existing one and refuses a misspelled name. An existing sandbox keeps its
original resources and node. `sh` needs an interactive terminal. In
scripts, use `exec` on a running sandbox; `exec -t` allocates a terminal,
and `-i` forwards stdin. Its exit status is the guest command's status;
125 indicates a CLI/connection failure (or a guest command that itself
returned 125), and a signal produces 128 + signal number.

Exactly one `cp` argument must be `sandbox:/absolute/path`. The destination
directory inside the guest must exist. Modes, times and symlinks are kept,
not ownership. Copies to a Linux host through sudo are root-owned.
Copies out reject traversal, hard links, devices and writes through
symlinks; see the [security notes](security.md).

The current image build includes coding agents; the older default pinned
image may lack those tools. See [image versions and agent setup](../image/README.md#coding-agents).
Authenticate agents inside the sandbox using the agent's own instructions;
Boxpool does not copy your host credentials automatically.

## Resources and persistence

Defaults are 2 CPUs, 2048 MiB RAM and 10 GiB disk. Override them only when
creating a sandbox, for example `sh --cpu 1 --mem 512m --disk 10g`.
Disk must fit the base image. Admission reserves one logical CPU and the
larger of 1 GiB or 10% host RAM; existing sandboxes keep their reservations
even when stopped or paused. Free capacity is conservative and is not
instantaneous CPU utilization. `rm` frees a reservation.

| Operation | What is kept |
| --- | --- |
| Exit shell | Running sandbox and disk |
| `stop work`, then `start work` | VM disk; Docker keeps only `/root`; memory is lost |
| `pause work`, then `resume work` | VM memory and disk where supported; Docker and unsupported Mac configurations stop instead and lose memory |
| `rm work` | Nothing from that sandbox's disk; requires it stopped, or `--force` |
| `down` | State directory; Linux sandboxes keep running, Mac VMs stop |

A sandbox created with `sh --restart always` boots again when the daemon
starts and finds it stopped, including after a host reboot. The default
`--restart no` leaves it stopped. Docker's memory and CPU limits are
host-enforced, but `free` inside a container shows host RAM. Docker disk
size is advisory: there is no portable per-volume quota; monitor the
Engine's disk. VMs reserve their full requested disk budget against free
space. See [resource details](development.md#sandbox-resources).

## Snapshots

```sh
boxpool snapshot work --name with-tools
boxpool snapshot ls
boxpool sh --from with-tools --name experiment
# Exit the guest shell, then:
boxpool snapshot rm with-tools
```

A snapshot copies the disk, not memory. A running sandbox stops for the
copy and starts again; its processes restart. Resume a paused sandbox
before snapshotting. VM snapshots contain the whole disk; Docker snapshots
contain only `/root`. Removing a snapshot leaves disks already cloned
from it intact.

Snapshots stay on their original node. Create, list, remove and restore
using that node's local CLI; `sh --from` does not transfer a snapshot
between nodes. Remote restore requires a token without `--sandbox`.

## Placement and pool health

On each contributing machine, set labels locally:

```sh
boxpool node label gpu,always-on
# Replace with an empty label list to clear them:
boxpool node label ""
```

The head sees changes at the next heartbeat. On the head:

```sh
boxpool sh --node gpu --name training
boxpool sh --os macos --arch arm64 --name mac-worker
boxpool ls --nodes
boxpool ls --json
```

These select host labels, host OS and architecture, while the guest stays
Linux. The head chooses a matching node with enough resources, preferring
the most free memory and then the fewest sandboxes. A name always returns
to its owning node. Mixed architectures need matching guest binaries.

Nodes report every five seconds. After 30 seconds without a heartbeat,
`ls` shows them and their last-known sandboxes as `unreachable`; records
are kept. A new heartbeat restores actual states. Capacity bars show free
admission capacity after reservations, not idle CPU. Unknown totals from
older nodes show `?`.

A Mac prevents idle sleep while sandboxes are running. Plug laptops into
power and keep their lids open: this does not prevent explicit or lid-close
sleep. `boxpool up --allow-sleep` opts out and saves that choice in the
launch agent; a later `boxpool up` without the flag restores prevention.
`doctor` and `ls --nodes` warn about battery power.

## Use the pool from a client laptop

A client does not contribute compute and needs no `up`, KVM or successful
`doctor`. Install the CLI. On the head, create an access token (use sudo
on Linux):

```sh
boxpool token create --scope cli --ttl 24h
```

On the client, replace the address and token with your head and the
printed access token:

```sh
boxpool login 192.168.1.10 'PASTE_ACCESS_TOKEN'
boxpool ls
boxpool exec hello -- uname -a
boxpool logout
```

The head must be reachable on TCP 7443; a tailnet IP works too. Login
pins its CA from the token and saves credentials for the current OS user.
Sandbox commands, snapshots, sandbox logs and `ls` use that head until
`logout`. Keep using the same user; sudo uses root's separate login.
Tokens default to no expiry unless `--ttl` is supplied. `--sandbox hello`
limits an access token to that sandbox; `api` has the same sandbox API
scope as `cli`, while `expose` tokens only open exposed URLs.

`up`, `down`, `join`, `node label`, `promote`, `token`, `audit`,
`logs --daemon`, `expose` and `desktop` still operate on the **local**
daemon. SSH into the head to manage its tokens, previews or desktop links.
`doctor` checks the current machine; login does not redirect it.

On the head, `boxpool token ls` shows access-token IDs and last use;
`boxpool token revoke <id>` revokes one. It can also revoke an unused join
token by its whole value. Tokens and exposed URLs are not replicated;
create new access tokens and URLs after failover.

## Failover

The head replicates node reports, join tokens and inventory to up to
three machines, including itself. `always-on` labeled nodes are preferred
as replicas. With three or more machines the replicas elect a new head.
With two, failover is manual:

1. Confirm the old head is unavailable.
2. On the surviving replica, run `boxpool promote` locally (sudo on Linux).
   It refuses if another replica answers.
3. Use the new head's address, create fresh access/join tokens as needed,
   and log clients in again. Recreate app/desktop URLs there.

A returning old head follows the promoted head as a node. Pool replication
is not a backup of sandbox disks, snapshots, access tokens, exposures or
audit history. Those stay on their original machine. Keep your own backups
of important files. See [failover trust limits](security.md#replicas-and-failover).

## State, logs and upgrades

| Platform | Installed service | State |
| --- | --- | --- |
| Linux | `boxpool.service` as root; binary `/usr/local/bin/boxpool` | `/var/lib/boxpool` |
| macOS | `run.boxpool.daemon` launch agent as you | `~/Library/Application Support/boxpool` |

State includes private keys, the database, cached images, snapshots and
sandbox disks. Do not delete it as a troubleshooting step. `BOXPOOL_HOME`
overrides state for foreground daemons and Mac launch agents; Linux `up`
uses the fixed system path and unsets the override. Clients must use the
same state directory as their local daemon. The login file is in the
user's config directory, or `BOXPOOL_HOME/login.json` when set.

```sh
boxpool logs --daemon
boxpool logs -f work
boxpool audit --limit 100
boxpool audit --json
```

Logs show VM serial console or Docker main-process output, not shell/exec
output. VM/daemon logs rotate at about 1 MiB and keep one previous file;
`logs` reads the current file. Ctrl-C stops following, not the sandbox.
The local head's audit log records successful actions and caller token IDs
for 90 days; see [audit guarantees](security.md#audit-log).

To upgrade, back up important sandbox files, install the new CLI with your
chosen method, then run `up` again using that binary. On Linux where a
user-local install differs from the service copy, use
`sudo "$(command -v boxpool)" up`; on a Mac use `boxpool up`. `version`
prints CLI and responding local daemon versions. An unchanged binary and
service definition make `up` a no-op. Linux adopts existing running
sandboxes; a Mac daemon restart stops its VMs. Restart them explicitly.
Image changes require recreating affected sandboxes; the CLI upgrade does
not replace their guest agents. For older Mac network attachments, follow
the [stop/start migration](security.md#macos) before upgrading.
