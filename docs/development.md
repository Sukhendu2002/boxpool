# Development setup

## macOS on Apple silicon

Use macOS 14+ with Xcode Command Line Tools (`xcode-select --install`) and
Go 1.24+. The Virtualization framework driver uses cgo. Intel Macs are
unsupported. Build and ad-hoc sign the daemon with the virtualization
entitlement; no Developer ID is needed for local development:

```sh
go build -o bin/boxpool ./cmd/boxpool
scripts/dev-sign.sh bin/boxpool
bin/boxpool up                    # as yourself, not with sudo
launchctl print gui/$(id -u)/run.boxpool.daemon | grep -E '^\s(state|pid) ='
bin/boxpool sh --timing --cpu 1 --mem 512m --disk 1g
uname -m                          # aarch64
echo kept > /root/stop-test
exit
bin/boxpool down
```

`up` installs the binary in `~/Library/Application Support/boxpool/bin`
and runs it as the launch agent `run.boxpool.daemon`, logging to
`~/Library/Logs/boxpool/daemon.log`. Rebuild, sign and run `up` again to
restart it on the new binary; with nothing changed it keeps the running
daemon. `down` stops it and removes its plist and keeps the state.

To run the daemon in the foreground instead, against a scratch state
directory, use `bin/boxpool daemon` and set the same `BOXPOOL_HOME` in the
terminal you run `sh` from:

```sh
export BOXPOOL_HOME="$(mktemp -d /tmp/boxpool-dev.XXXXXX)"
bin/boxpool doctor
bin/boxpool daemon
```

The first daemon start fetches the pinned arm64 kernel and rootfs (about
45 MB); subsequent starts reuse them. `sh --timing` prints time to prompt.
APFS is required: each sandbox gets a copy-on-write `clonefile` disk.
The daemon uses the same CPU, memory and disk admission as Linux; macOS
available memory comes from `kern.memorystatus_level`. Guest ext4 grows
online through `EXT4_IOC_RESIZE_FS`, invoked with the pinned image's Perl.
The kernel prevents opening a mounted block device for writing, so the
image's `resize2fs` cannot perform this growth.

On macOS 26 and later each sandbox gets outbound IPv4 through NAT on a
network of its own, which needs the macOS 26 SDK (Command Line Tools 26 or
later) at build time; otherwise sandboxes have no network. See [security.md](security.md). Pause saves the Virtualization framework machine state on macOS 14+ and Resume restores it; unsupported configurations fall back to Stop. See [the M3 benchmark](benchmarks/m3-macos-pause.md). Stop syncs and freezes the guest rootfs,
then triggers sysrq power-off; guest programs get no exit signal first
(#91). If that fails, Stop requests a framework shutdown, then forces it,
reporting `backend.ErrUncleanStop`. `boxpool down` or Ctrl-C on a
foreground daemon stops its VMs; their disks remain for the next start.
The agent's plist gives that shutdown 60 seconds (`ExitTimeOut`) before
launchd kills the daemon. The framework's VM helpers also end when the
daemon process exits. Check for leftovers after exit:

```sh
ps -axo pid,comm | grep '[c]om.apple.Virtualization.VirtualMachine'
```

Unit tests need no signing. The integration test needs a signed test
binary and real virtualization, so it runs locally rather than in CI:

```sh
go vet ./...
go test -race ./...
go test -c -tags integration -o /tmp/vz.test ./internal/backend/vz
scripts/dev-sign.sh /tmp/vz.test
/tmp/vz.test -test.v -test.timeout 5m
go test -tags integration -run TestLaunchAgent -v ./cmd/boxpool
```

`TestLaunchAgent` runs the launchd half of `up` and `down` with the label
`run.boxpool.daemon.test` and a temporary state directory, so it needs no
free-disk check and leaves your agent alone. It builds and signs its own
binaries, refuses an unsigned one, checks that a second `up` keeps the
daemon's PID, that a new binary or plist restarts it, that a VM runs under
it, and that `down` stops it, removes the plist and leaves no VM helper.

It checks arm64 exec, resource sizes, bounded console output, persistence,
clean and forced stops, driver Close, and removal. Install `e2fsprogs`
(`brew install e2fsprogs`) to also check the stopped disk with `e2fsck -fn`.
Artifacts are cached in `~/Library/Caches/boxpool-test`, or
`BOXPOOL_TEST_HOME` when set. Boot times include waiting for the agent and
growing ext4. CI builds and runs unit tests with cgo on macOS; Linux and
Windows do not compile the framework binding.

## Pool transport

`internal/transport.Transport` provides `Listen`, `Dial` and the address
advertised in join tokens. `Direct` and `Tailscale` share system TCP sockets;
the installed tailscaled supplies the routes for Tailscale or Headscale.
The head's wildcard listener serves both networks. Pool joins, heartbeats,
request tunnels, replica probes and Raft connections all use this boundary,
with CA pinning or mutual TLS layered above it. Existing join token formats
and saved credentials remain valid. A working join address is retained
when Raft advertises that same head under another address; failed
heartbeats try the elected leader and saved peers before adopting a new
destination.

The contract suite exercises both implementations on loopback without an
auth key: the system client is authenticated separately, rather than by
embedding tsnet. It checks cancellation, listener shutdown, mutual TLS,
rejection of unauthenticated clients and bounded TLS handshakes:

```sh
go test ./internal/transport -v
go test ./internal/daemon -run 'TestHeadAddressAfterReplication|TestFailover|TestPromote' -count=1
go test ./cmd/boxpool -run 'TestTokenCmd|TestTokenCreateTailscale|TestTailnetAddrTimeout' -count=1
```

To run the same contract on a real tailnet interface, set
`BOXPOOL_TEST_TAILNET_IP` to this machine's connected Tailscale IP and run
`go test ./internal/transport -run TestTailnetContract -count=1 -v`.
That check is skipped when the variable is unset; it uses an ephemeral
port and requires no changes to the installed client's configuration.

The issue #44 cross-network acceptance check still needs two physical
networks. A passing local tailnet contract does not certify it:

1. Connect the home head and laptop to the same tailnet. Allow pool TCP
   port 7443 in tailnet policy and host firewalls.
2. On the head, run `boxpool token create --join --transport tailscale`
   (`sudo boxpool token create --join --transport tailscale` on Linux).
3. Move the laptop to a phone hotspot, keeping its system tailnet client
   connected. Run `boxpool join <token>` (with `sudo` on Linux). Expect exit
   0 and `joined the pool at <head-tailnet-ip>:7443 as node ...`. Then label
   the laptop with `boxpool node label laptop` (with `sudo` on Linux).
4. On the head, run `boxpool ls --nodes`, then `boxpool sh --node laptop`
   (with `sudo` on Linux). Expect a ready laptop and a working sandbox shell.
   Repeat after at least 30 seconds, and after restarting the laptop daemon,
   to exercise new heartbeats and request tunnels after replication starts.

## Pool status, console logs and Mac power checks

Use a signed binary and a scratch `BOXPOOL_HOME`, so these checks do not
restart your installed daemon. Start `bin/boxpool daemon` in one terminal;
use the same `BOXPOOL_HOME` in another:

1. Create two VMs with `bin/boxpool sh --name power-a --cpu 1 --mem 512m --disk 1g`
   and `power-b` with the same flags; exit each shell without stopping it.
2. `pmset -g assertions | grep 'boxpool: running sandboxes'` must show one
   `PreventUserIdleSystemSleep` assertion belonging to the daemon PID.
3. `bin/boxpool logs -f power-a` must show a marker sent from another terminal:
   `bin/boxpool exec power-a -- sh -c 'echo marker > /dev/console'`.
   Ctrl-C must stop following. `bin/boxpool logs --daemon` must show startup.
4. Stop `power-a`: the assertion must remain. Pause `power-b`: it must
   disappear. Resume `power-b`: it must reappear. Stop it: it must disappear.
5. Restart the scratch daemon with `--allow-sleep`, then start `power-a`:
   no boxpool assertion should appear. Stop/remove the scratch VMs and daemon.
   `boxpool up --allow-sleep` should put that flag in its launch agent's
   `ProgramArguments`; plain `boxpool up` restores the default.
6. Unplug the laptop: `bin/boxpool doctor` and `bin/boxpool ls --nodes`
   must warn `on battery`; reconnect AC and the warning must disappear.
   For a joined Mac, check on the head after the next heartbeat (five seconds).
7. With a second node joined, compare `ls` and `ls --nodes` to each node's
   local list, then disconnect that node for over 30 seconds: its sandboxes
   must remain `unreachable`; its log requests must fail naming that node.

On 2026-10-29, macOS/arm64 testing with two real 512 MiB VMs passed steps
1–5's foreground-daemon checks, including console snapshot/follow, daemon
logs, stop/pause/resume assertion transitions and `--allow-sleep`. A repeat
also verified a clean daemon exit with an active `logs -f --daemon` session.
Unit tests block create/start/resume before guest readiness to check early
assertion acquisition and failed-start release; routed followers are also
cancelled on head shutdown. The launch-agent flag has unit coverage. The machine was on AC: unplug/replug
and a two-physical-node check were not run. The in-process mTLS/tunnel pool
test covers remote log snapshot/follow and unreachable sandbox retention.
Idle-sleep prevention does not prevent explicit or lid-close sleep; keep
laptops plugged in with lids open unless macOS clamshell requirements are met.

## Releases

Pushing a `vX.Y.Z` tag (or `vX.Y.Z-suffix`, published as a pre-release) on a
`main` commit runs the [release workflow](../.github/workflows/release.yml).
It builds static `boxpool-linux-amd64` and `boxpool-linux-arm64`, and
`boxpool-darwin-arm64` with the macOS 26 SDK. The Mac binary is ad-hoc signed
with the hardened runtime and `scripts/vz.entitlements`. It is **not Developer
ID signed or notarized**. This release path requires no paid Apple Developer
membership or Apple repository secrets; Developer ID signing and notarization
are deferred. Last, the workflow uploads all three binaries and
`checksums.txt` to the GitHub release using the automatic `GITHUB_TOKEN`.
PRs that touch the workflow build and verify the same signatures without
publishing. GitHub's hosted Macs cannot run VMs ("Virtualization is not
available on this hardware"), so run the integration tests above on your Mac
before tagging.

A tag push creates the release with the workflow's token, then explicitly
calls the [guest image workflow](../.github/workflows/image.yml) to build,
test and publish matching guest images. GitHub does not emit another release
event for that token, so the explicit call is required. A release created
with `gh release create` or the web UI triggers the image workflow directly;
the tag workflow only uploads binaries into that existing release. Include
the ad-hoc signing and Gatekeeper notice below in manually created release
notes too; workflow-created releases include a link to these instructions.

Images must advertise and exercise the agent's `tcp` feature before
publication: both OCI architectures test a real loopback HTTP request over
`GET /v1/tcp`, and the KVM smoke tests that path in the VM. Publishing does
not change the default image pins. Follow the
[image rollout checklist](expose.md#guest-images) to update the verified VM
checksums/sizes and Docker index digest after publication.

Check a release:

```sh
release_dir=$(mktemp -d /tmp/boxpool-release.XXXXXX)
gh release download v0.1.0-alpha.1 --repo Sukhendu2002/boxpool \
  --pattern 'boxpool-*' --pattern checksums.txt --dir "$release_dir"
cd "$release_dir"
shasum -a 256 -c checksums.txt
file boxpool-linux-amd64 boxpool-linux-arm64   # both statically linked
chmod +x boxpool-darwin-arm64
codesign --verify --strict --verbose=2 boxpool-darwin-arm64
codesign -d --verbose=4 boxpool-darwin-arm64   # Signature=adhoc
codesign -d --entitlements - boxpool-darwin-arm64
# Expect com.apple.security.virtualization = true in the entitlement output.
```

All three checksums must say `OK`. The ad-hoc signature verifies the binary's
integrity and carries the virtualization entitlement; it does not establish
a trusted publisher identity or Apple notarization. `spctl` rejection is
expected for this release and is not a signature verification failure.

macOS may block a browser-downloaded binary because it is not notarized.
After verifying the download's source and checksums, try opening it, then
use **System Settings → Privacy & Security → Open Anyway** if offered
([Apple's instructions](https://support.apple.com/102445)). For a trusted
standalone CLI download, you can instead remove quarantine from that file
only, if present:

```sh
if xattr -p com.apple.quarantine boxpool-darwin-arm64 >/dev/null 2>&1; then
  xattr -d com.apple.quarantine boxpool-darwin-arm64
fi
./boxpool-darwin-arm64 version         # boxpool v0.1.0-alpha.1
./boxpool-darwin-arm64 doctor
```

Leave system-wide Gatekeeper protection enabled. Building from source and
using `scripts/dev-sign.sh` is another free option.

## Linux and KVM

For building the shared guest kernel/rootfs/OCI image, required tools,
Firecracker/vsock smoke checks and release publication, see the
[base image guide](../image/README.md).

Use a Linux x86_64 or aarch64 machine for the Linux backends. Start with a
spare machine on your LAN; use a nested VM for convenience, or cloud hardware
as a last resort. Windows hosts and automated provisioning are out of scope.

`Backend: firecracker` means doctor
could open `/dev/kvm` for reading and writing; it does not boot a microVM or
check for a Firecracker executable. Without KVM, doctor picks `Backend:
docker`, the fallback, when the Docker Engine API answers on
`/var/run/docker.sock` (or a `unix://` `DOCKER_HOST`), and prints a
`Warning:` line: containers share the host's kernel, so isolation is weaker.
Set `BOXPOOL_BACKEND=docker` or `firecracker` to force a backend; doctor
fails if the forced one is not usable. The daemon logs the same warning at
start and serves it in `/v1/info`.

## Option A: a spare Linux machine over SSH

Enable Intel VT-x or AMD-V in the machine's BIOS/UEFI on x86_64. Install a
current Linux distribution with a KVM-enabled kernel and a regular user with
`sudo` access. These commands target Ubuntu 24.04+ or Debian 13+.

At the Linux console (or in an existing SSH session):

```sh
sudo apt update
sudo apt install -y openssh-server git golang-go docker.io e2fsprogs iproute2 nftables
sudo systemctl enable --now ssh docker
```

On Fedora, use these instead:

```sh
sudo dnf install -y openssh-server git golang moby-engine docker-cli e2fsprogs iproute nftables
sudo systemctl enable --now sshd docker
```

These use distribution Docker packages ([Debian](https://packages.debian.org/trixie/docker.io),
[Fedora](https://packages.fedoraproject.org/pkgs/moby-engine/)). If Docker CE
is already installed, keep it rather than mixing package sources. Allow SSH
through the host firewall on your trusted LAN if necessary.

Set up group access on Linux:

```sh
sudo groupadd -f kvm
sudo groupadd -f docker
sudo usermod -aG kvm,docker "$USER"
```

The [Docker group grants root-level privileges](https://docs.docker.com/engine/install/linux-postinstall/);
add only trusted development users. Log out completely and reconnect from
the Mac, replacing `dev@linux-box` with your Linux user and LAN hostname/IP:

```sh
ssh dev@linux-box
```

In that new Linux session:

```sh
ls -l /dev/kvm
id
docker run --rm hello-world
```

Expect `/dev/kvm` to have group `kvm` with group read/write permission, `id`
to include `kvm` and `docker`, and Docker to print its hello-world success
message. Continue with **Build and verify on Linux**, or copy a Mac build.

## Option B: a nested Linux VM on a Mac

This path requires **Apple M3 or newer, macOS 15 or newer, and Lima's `vz`
VM type**. M1/M2 Macs cannot use this path. Check on the Mac:

```sh
sysctl -n machdep.cpu.brand_string
sw_vers -productVersion
which limactl
```

If needed, install Lima using its [official installation instructions](https://lima-vm.io/docs/installation/).
With Lima 2.1+ installed, create a new Ubuntu VM from the default template:

```sh
limactl start --name=boxpool-dev --vm-type=vz --nested-virt template:default
limactl shell boxpool-dev
```

Choose “Proceed with the current configuration” if prompted. The exact YAML
settings are `vmType: vz` and `nestedVirtualization: true`; see Lima's
[configuration reference](https://github.com/lima-vm/lima/blob/master/templates/default.yaml)
for the hardware/OS requirements and its
[`start` reference](https://lima-vm.io/docs/reference/limactl_start/) for the flags.
`--nested-virt` was added in [Lima 2.1](https://github.com/lima-vm/lima/releases/tag/v2.1.0).

**Inside the VM**, run option A's Ubuntu package commands and group setup.
Then `exit` the guest shell and run `limactl shell boxpool-dev` again on the
Mac to pick up the new groups. Run `ls -l /dev/kvm`, `id`, and the Docker
hello-world check inside the VM. The Mac itself has no `/dev/kvm`.

Build in the guest's own home directory (`cd ~`), since Lima mounts the
Mac's home [read-only by default](https://lima-vm.io/docs/usage/#home-directory).
Continue below. Nested VM boot timings are **not reference performance
numbers**; measure boot performance on native Linux hardware.

## Option C: cloud hardware as a last resort

Use a Linux bare-metal instance (for example, AWS
[`i3.metal`](https://aws.amazon.com/ec2/instance-types/storage-optimized/)),
or an instance type whose provider explicitly supports nested virtualization
(see [Google Compute Engine's requirements](https://docs.cloud.google.com/compute/docs/instances/nested-virtualization/overview)).
A normal VM or dedicated-tenancy VM does not by itself guarantee KVM access.
Enable nested virtualization using the provider's instructions before booting
the guest. SSH in, follow option A's Linux setup, and check `/dev/kvm` inside
the instance. Stop/delete chargeable resources when finished.

## Build and verify on Linux

From a Linux session in the machine or VM:

```sh
cd ~
git clone https://github.com/Sukhendu2002/boxpool.git
cd boxpool
go version
go vet ./...
go test ./...
go build -o bin/boxpool ./cmd/boxpool
./bin/boxpool doctor
```

The repo requires Go 1.24+. Ubuntu 24.04's packaged Go is older, but Go 1.21+
[automatically downloads the required toolchain](https://go.dev/doc/toolchain)
when running in this repo with the default `GOTOOLCHAIN=auto`. If automatic
downloads are disabled or unavailable, [install Go 1.24+](https://go.dev/doc/install)
before building. Use `git switch <your-branch>` after cloning to test your work.

Expected doctor output on Linux/amd64 with Docker installed (spacing elided):

```text
boxpool doctor: linux/amd64

  ✓ /dev/kvm                       present and accessible
  ✓ CPU virtualization (vmx/svm)   supported
  ✓ docker engine (fallback)       Docker 28.0.4, API 1.47, on /var/run/docker.sock

Backend: firecracker
```

On Linux/arm64 the header says `linux/arm64` and there is **no vmx/svm check**.
Backend selection depends on `/dev/kvm` access, not that CPU flag check.
Run doctor as your regular user, without `sudo`, to check your actual access.

## Build on the Mac, copy to Linux

From the repo root on the Mac, build for the **destination** architecture
(`uname -m` on Linux: `x86_64` → `amd64`, `aarch64` → `arm64`):

```sh
GOOS=linux GOARCH=amd64 go build -o bin/boxpool-linux-amd64 ./cmd/boxpool
GOOS=linux GOARCH=arm64 go build -o bin/boxpool-linux-arm64 ./cmd/boxpool
```

Copy the matching binary, for example for x86_64 Linux:

```sh
scp bin/boxpool-linux-amd64 dev@linux-box:boxpool
ssh dev@linux-box 'chmod +x ./boxpool && ./boxpool doctor'
```

For an arm64 destination, copy `bin/boxpool-linux-arm64` instead. For Lima,
copy the arm64 binary from the mounted Mac path into the guest's home:

```sh
limactl shell boxpool-dev
cp /Users/YOUR_MAC_USER/PATH_TO_REPO/bin/boxpool-linux-arm64 ~/boxpool
chmod +x ~/boxpool
~/boxpool doctor
```

Replace the Mac path above. The result must include `✓ /dev/kvm` and
`Backend: firecracker`. A successful exit code alone is insufficient: doctor
also exits successfully when it selects `Backend: docker`.

## Linux testing for each PR

The existing `ci` workflow runs vet, unit tests, and builds on Linux and
macOS. The [Linux KVM workflow](../.github/workflows/linux-kvm.yml) also runs
on every PR and push to `main`, using an Ubuntu 24.04 x86_64 hosted runner.
It checks the real `/dev/kvm` device, grants the runner KVM group access,
and uses `sg kvm` to apply that group in a new shell without running doctor
as root. It requires both accessible KVM and `Backend: firecracker`; missing
KVM or a Docker fallback fails the job instead of silently skipping the test.

Standard hosted runners are [free for public repositories](https://docs.github.com/en/actions/reference/runners/github-hosted-runners).
GitHub documents [Linux KVM acceleration](https://github.blog/changelog/2024-04-02-github-actions-hardware-accelerated-android-virtualization-now-available/),
but general nested VMs are [experimental](https://docs.github.com/en/actions/concepts/runners/github-hosted-runners).
If KVM disappears from a runner, inspect the failed job and use option A, B,
or C for KVM verification. A green unit-test job alone does not replace it.

For upcoming Linux backend PRs, add repeatable integration tests to CI as
the features land: actual VM boot/stop, guest commands, networking, and
cleanup. Today's doctor job only verifies preflight; it does not cover those
future behaviors, arm64 KVM, or native-hardware boot timings. Use a persistent
SSH-accessible Linux machine for interactive debugging, multi-node tests,
and performance measurements. A repaired laptop can serve that role without
needing its screen once SSH is configured.

Once this workflow is on `main`, it can also be [run manually](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/manually-run-a-workflow)
from **Actions → Linux KVM → Run workflow**, selecting the branch to test.

### Your own Linux machine, before you push

`scripts/test-on-linux.sh` copies the working tree (uncommitted changes
included) to your Linux machine over SSH and runs vet, tests, a build and
`boxpool doctor` there, requiring `Backend: firecracker`. Set
`BOXPOOL_LINUX_HOST` to its SSH host, e.g. in your shell profile. The remote
needs `rsync`, Go on `PATH` or in `~/.local/go`, and read/write access to
`/dev/kvm` for your user.

CI runs the Firecracker integration tests on every PR and push to `main`
in the `integration` job of the [Linux KVM workflow](../.github/workflows/linux-kvm.yml):
the driver and `boxpool sh` tests unprivileged, then the networking test as
root (GitHub's runners have passwordless sudo and Docker's `FORWARD` DROP
policy). It fails if a test skips or leaves a `firecracker` process or `bp-*`
tap behind, and uploads the logs on failure. The Docker tests run in the
[Docker fallback workflow](../.github/workflows/docker.yml).

The Firecracker driver's integration test boots real VMs and is behind a
build tag. On a machine with `/dev/kvm`:

```sh
go test -tags integration -v ./internal/backend/firecracker/...
```

It downloads the artifacts once into `~/.cache/boxpool-test` (override with
`BOXPOOL_TEST_HOME`) and keeps them between runs. Because that directory is
fixed, a run you interrupt leaves its VM behind, and the next run kills it.
Without `/dev/kvm` the test skips.

To test agent changes before an image release, set `BOXPOOL_TEST_ROOTFS` to
a `rootfs-<arch>.ext4.zst` in a directory of its own, for example one from
`image/build.sh`, or the pinned rootfs with a new agent written into it:

```sh
CGO_ENABLED=0 go build -o /tmp/boxpool-agent ./cmd/boxpool-agent
mkdir /tmp/img && cp --sparse=always ~/.cache/boxpool-test/artifacts/rootfs-amd64.ext4 /tmp/img/
debugfs -w -f - /tmp/img/rootfs-amd64.ext4 <<'EOF'
cd /usr/local/bin
rm boxpool-agent
write /tmp/boxpool-agent boxpool-agent
sif boxpool-agent mode 0100755
sif boxpool-agent uid 0
sif boxpool-agent gid 0
EOF
zstd --rm /tmp/img/rootfs-amd64.ext4
BOXPOOL_TEST_ROOTFS=/tmp/img/rootfs-amd64.ext4.zst go test -tags integration -v ./internal/backend/firecracker/...
```

`TestIntegration` checks Stop through `POST /v1/shutdown` when the agent
lists that feature (a SIGTERM trap's marker survives, a process ignoring
SIGTERM is killed after the grace period, the disk is clean) and through
the freeze and sysrq fallback otherwise.

`TestIntegrationSh` runs commands through a real daemon, as `boxpool sh`
does, on each backend, with a temporary state directory that links to the
same artifact cache:

```sh
go test -tags integration -v -run TestIntegrationSh ./internal/daemon/
```

Its Firecracker case skips without `/dev/kvm`, its Docker case without a
reachable engine (or fails, with `BOXPOOL_TEST_REQUIRE_DOCKER=1`). Each
removes its sandbox afterwards.

Daemon shutdown closes exec sessions and waits for request cleanup before
closing the store or releasing its lock. If cleanup exceeds the shutdown
grace period, it logs the number of requests still running and keeps
waiting. A backend operation that ignores cancellation can therefore delay
shutdown indefinitely; returning early would let rollback use a closed
store or race a replacement daemon.

The networking test (`TestIntegrationNetwork`) needs root, since it creates
tap devices and firewall rules; without root it skips. Build the test binary
as yourself and run only it with `sudo`, pointing it at your artifact cache:

```sh
go test -c -tags integration -o /tmp/fc-it.test ./internal/backend/firecracker
sudo env BOXPOOL_TEST_HOME="$HOME/.cache/boxpool-test" /tmp/fc-it.test -test.v -test.run TestIntegrationNetwork
```

Add `BOXPOOL_TEST_APT=1` to also install `curl` and `ping` in a sandbox from
the image's Debian mirror and check HTTPS (HTTP 200 or 301) and an actual
external ICMP reply. The test checks command exit codes and full responses.
Keep `BOXPOOL_TEST_HOME` fixed between crash-recovery runs: its
`network-state.db` contains the allocations needed to verify TAP ownership.
For independent runs, use separate homes and symlink their `artifacts`
directory to the existing artifact cache. Do not run network tests concurrently:
the firewall table is shared. When a run ends, its owned TAPs are gone,
`sudo nft list table inet boxpool` fails and
`sudo iptables -S | grep boxpool` prints nothing. Unrelated `bp-*` interfaces
are preserved; that prefix remains reserved for firewall matching.

Root cleanup and collision regressions run only in disposable network
namespaces (no KVM or external access needed):

```sh
sudo /tmp/fc-it.test -test.v -test.run '^TestNetwork' -test.timeout=2m
```

They cover foreign devices, owned orphans, creation failures/crashes,
canceled Stop/Remove, genuine cleanup failures and cleanup deadlines. Each
root regression creates its own namespace and refuses the host namespace.

### Docker fallback

The Docker driver's integration test runs real containers of the published
base image and needs a rootful Docker Engine your user can reach (root, or
the root-equivalent `docker` group). It does not need KVM, and ignores it:

```sh
go test -tags integration -v -run TestIntegration ./internal/backend/docker/
```

It creates two sandboxes in a fresh temporary state directory and checks the
hardening as the engine reports it and from inside, the agent socket's
permissions, that the two cannot connect to each other (with listener
controls that can be reached), that a guest swapping its agent socket for
a symlink to the other's cannot redirect the daemon, persistence across Stop and Start, discovery
by a new driver, and that failed and duplicate creates leave nothing. It
touches only resources labelled with that state directory and fails if any
are left. Without a reachable engine it skips; set
`BOXPOOL_TEST_REQUIRE_DOCKER=1` to make that a failure, as the
[Docker fallback workflow](../.github/workflows/docker.yml) does on every PR.
To inspect a sandbox by hand:

```sh
docker inspect <container> --format '{{.HostConfig.CapDrop}} {{.HostConfig.SecurityOpt}} {{.HostConfig.ReadonlyRootfs}}'
# [ALL] [no-new-privileges:true] true
```

[Docker Sandboxes](https://docs.docker.com/ai/sandboxes/) is a different
product: it runs microVMs and [needs KVM on Linux](https://docs.docker.com/ai/sandboxes/install/),
so it is no fallback for hosts without it. This backend uses ordinary
Docker Engine containers; `internal/backend/docker` documents its hardening
and limits.

The agent socket's defence against symlinks is Linux-only and unit-tested
without an engine (`dial_linux_test.go`); `go test ./internal/backend/docker/`
on Linux runs it.

### Sandbox resources

Both drivers and the store default an unspecified Spec to **2 CPUs,
2048 MiB memory and 10 GiB disk**. Zero selects a default; negative values
and byte-conversion overflow are rejected. `boxpool sh --cpu 1 --mem 512m
--disk 1g` passes those requests through the daemon to the selected driver.

Admission leaves **one logical CPU and max(1024 MiB, 10% of host RAM)** for
the host. CPU admission uses the daemon's allowed CPU affinity. Memory must
fit Linux `MemAvailable`, minus the host reserve and every existing
sandbox's full memory commitment. This conservatively counts resident
sandbox RAM twice, preserving capacity for stopped or idle sandboxes under
host memory pressure. The budget is not an idle-CPU measurement and cannot
prevent other host programs from using resources after admission. Run the
daemon directly on the Linux host.

Every created sandbox, including stopped ones, keeps its reservation until
Remove. Both backends and concurrent processes with the **same state
directory** share `resources.lock`; private `resources.json` manifests
survive restarts. A failed cleanup keeps its manifest until Remove succeeds.
Per-sandbox files in `resource-locks/` serialize lifecycle changes; keep
those stable lock files even after a sandbox is removed.
Keep those files with the sandbox state, and inspect leftover resources
before removing reservations manually. Corrupt manifests fail admission.
Use one state directory per node: independent `BOXPOOL_HOME` directories
do not coordinate their commitments. Existing Firecracker specs and Docker
containers are accounted for when upgrading. Initialize the Docker backend
to import legacy Docker limits before switching a state directory to
Firecracker; unknown Docker allocations fail admission. Existing Docker
settings stay unchanged until the sandbox is recreated.

Firecracker sets the vCPU count and memory in its machine config, then
boots a private ext4 rootfs grown to the requested GiB with host
`resize2fs` (`e2fsprogs`). It refuses shrinking below the base image size.
Disk admission conservatively charges **the entire disk budget of every
VM against current free space**, including already allocated blocks. This
protects future sparse growth and reflink copies at the cost of sometimes
rejecting early. The cached image stays unchanged. `nproc` matches the CPU
request; `/proc/meminfo` and `free -m` show usable guest RAM, slightly below
the configured amount because of the guest kernel's reservations.
Newly built images include `procps` for `free`; the pinned alpha.1 image
predates that package, so read `/proc/meminfo` until a new image is released.

Docker sets CPU quota, CPU affinity, memory limit and equal memory-plus-swap
limit (no swap). `nproc` matches new containers' CPU requests. Its memory
limit is enforced by cgroups: **`free -m` and `/proc/meminfo` still show
host memory**. This is documented rather than adding `/proc` virtualization.
On cgroup v2, verify from inside the sandbox:

```sh
nproc                         # 1 for CPUs: 1
cat /sys/fs/cgroup/memory.max   # 536870912 for MemoryMB: 512
cat /sys/fs/cgroup/cpu.max      # quota == period for one CPU
```

Docker disk size is a **documented limitation**: a plain named volume has
no portable per-volume quota. `DiskGB` is recorded but is advisory; Docker
disk growth is neither bounded nor reserved. Monitor the Docker Engine's
data filesystem. No host mounts or quota configuration are added.

Tests: `go test -race ./internal/resources ./internal/backend/...` covers
defaults, capacity boundaries, memory pressure, stopped/legacy allocations,
concurrent creates, restart accounting, rollback and removal. Linux also
exercises real ext4 growth when e2fsprogs is installed. The existing
Firecracker integration test requests 1 CPU, 512 MiB and 1 GiB and verifies
agent-visible CPU, memory, block-device and filesystem sizes. The Docker
integration test verifies 1 CPU/512 MiB in Engine inspect and from inside
the memory/CPU cgroups. Both reject an excessive memory request before
creating resources.

To run it on every `git push`, enable the repo's hooks once:

```sh
git config core.hooksPath .githooks
```

The hook skips with a warning when `BOXPOOL_LINUX_HOST` is unset or the machine is unreachable; use
`git push --no-verify` to skip it on purpose. There is deliberately no
self-hosted GitHub runner: on a public repo, a PR could run its own code on
that machine. Only you, over SSH, can.

## Troubleshooting doctor

- `/dev/kvm` — `missing: enable virtualization in BIOS or load the kvm module`:
  enable VT-x/AMD-V in firmware on x86_64. Try `sudo modprobe kvm` then
  `sudo modprobe kvm_intel` (Intel) or `sudo modprobe kvm_amd` (AMD). In a VM,
  enable nested virtualization on the outer host and restart the VM. On arm64,
  use a kernel with KVM support; the x86 modules do not apply. Recheck the
  device **inside the guest**.
- `/dev/kvm` — `not accessible: add your user to the kvm group`: repeat the
  group setup, fully log out/in, and check `id` and `ls -l /dev/kvm`. If group
  ownership or permissions differ, check the distribution's udev rules for
  `/dev/kvm` (expected `root:kvm`, mode `0660`); do not make it world-writable.
- `CPU virtualization (vmx/svm)` — `not reported by the CPU`: on amd64, check
  firmware settings or whether the outer hypervisor exposes virtualization
  extensions. This check is absent on arm64.
- `docker engine (fallback)` — `no Docker Engine is listening`: install
  Docker and start it (`sudo systemctl start docker`). `permission denied`:
  recheck Docker group membership and log in again. Doctor reads only
  `DOCKER_HOST` (`unix://` URLs), not Docker contexts.
- `Backend: none`: neither accessible KVM nor a reachable Docker Engine, or
  `BOXPOOL_BACKEND` names one that is not usable. Fix KVM for Firecracker
  development; a Docker fallback does not meet that goal.
