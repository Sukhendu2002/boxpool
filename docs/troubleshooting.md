# Troubleshooting

Run `boxpool doctor` on the machine that will host sandboxes. On Linux,
check the installed service's view with `sudo /usr/local/bin/boxpool doctor`
(or `sudo "$(command -v boxpool)" doctor` before `up` installs it).
On a Mac, run as yourself, without sudo. A client-only laptop does not
need to pass doctor to use `login`.

## Read the doctor report

`✓` means a check passed; `✗` means it failed; `!` is a warning. The final
`Backend:` and exit status determine readiness. Optional checks can fail
while doctor exits 0: Docker Engine is optional if Firecracker works,
and KVM is optional when using Docker fallback. A required check such as
kernel version or disk space still blocks startup. Doctor does not check
all service/networking prerequisites: also install systemd, `ip` and `nft`
on Linux as in the [quickstart](../README.md#1-install-on-both-machines).

Use the **check name printed on your machine** below. After fixing it,
rerun doctor in the same account, then rerun `up`.

| Check / detail | Action |
| --- | --- |
| `Linux kernel`: `kernel 5.10 or newer required` | Upgrade the host kernel and reboot. Check `uname -r` on the actual host/VM running Boxpool. |
| `VM host`: `nested virtualization may be unavailable or slow` | Enable nested virtualization on the outer hypervisor if you want KVM. This warning alone does not block startup; check `/dev/kvm` inside the VM. |
| `/dev/kvm`: `missing: enable virtualization in BIOS or load the kvm module` | On x86, enable VT-x/AMD-V in firmware; try `sudo modprobe kvm` and the matching `kvm_intel` or `kvm_amd` module. In a VM, enable nesting outside it and restart it. arm64 needs a KVM-capable kernel; x86 modules do not apply. |
| `/dev/kvm`: `not accessible: add your user to the kvm group` | For user-mode checks, use `sudo usermod -aG kvm "$USER"`, fully log out/in, then check `id` and `ls -l /dev/kvm`. Typical ownership is `root:kvm`, mode `0660`; do not make it world-writable. For the root service, rerun doctor with sudo. |
| `CPU virtualization (vmx/svm)`: `not reported by the CPU` | Check x86 firmware settings or the outer hypervisor's CPU exposure. This check is absent on arm64. |
| `docker engine (fallback)`: `no Docker Engine is listening` / permission error | If KVM works, this is optional. For fallback, install/start local rootful Docker Engine (`sudo systemctl start docker`) and check `sudo docker info`. The Boxpool daemon must run as root, with `ip` and `nft`. Docker probing reads Unix-socket `DOCKER_HOST`, not Docker contexts; remote/rootless engines are unsupported for daemon networking. |
| `BOXPOOL_BACKEND`: `unknown backend` / `forced but not usable here` | Unset an accidental `BOXPOOL_BACKEND` in the process that starts the daemon, or choose usable `firecracker` or `docker`. It does not apply to the Mac VM driver. |
| `resize2fs`: `missing: install e2fsprogs` | Debian/Ubuntu: `sudo apt-get install e2fsprogs`; Fedora: `sudo dnf install e2fsprogs`. Rerun under sudo if the tool is only on root's PATH. |
| `Apple silicon`: `Intel Macs are not supported` | Use an Apple silicon Mac or a supported Linux machine. Check `uname -m`; run a native arm64 release rather than an Intel binary under Rosetta. |
| `macOS 14 or later` | Upgrade to macOS 14+. For sandbox internet use macOS 26+ and a release built with its SDK. |
| `power source` | On a Mac laptop, plug in power for reliable hosting. The battery warning is informational. |
| `free disk`: `need 20 GB` | Free space on the filesystem at the printed path. When the state directory does not yet exist, doctor measures its nearest existing parent. Do not delete sandbox disks or resource manifests to make space. Linux service state is `/var/lib/boxpool`; Mac state is `~/Library/Application Support/boxpool`. |
| `free disk`: `could not check` / `is not a directory` | Check the state path and its parent permissions. On Linux retry with sudo. An accidental `BOXPOOL_HOME` can point at a file or the wrong filesystem; Linux `up` always uses `/var/lib/boxpool`. |
| `supported OS` / `Backend: none` | No usable backend was selected. Fix the failed checks above; Windows and Intel Mac hosting are unsupported. |

## Install or startup fails

- **`boxpool: command not found`:** add the installer's printed directory to
  PATH. For `~/.local/bin`, run `export PATH="$HOME/.local/bin:$PATH"`.
  Sudo can use a different PATH: use `sudo "$(command -v boxpool)" up`
  for first setup, then `/usr/local/bin/boxpool` for the installed service.
- **macOS blocks the binary / missing virtualization entitlement:** use the
  [release verification and Gatekeeper steps](development.md#releases).
  Source builds need `scripts/dev-sign.sh bin/boxpool`. Run `up` as your
  logged-in user. Do not turn off Gatekeeper globally.
- **Linux `up` needs systemd:** run on the actual systemd host, not inside
  a container. For development, [foreground daemon setup](development.md)
  is available, with different privilege/networking constraints.
- **Download, checksum or startup timeout:** first start fetches roughly
  60 MB on Linux VMs or 45 MB on Macs. Check internet access to GitHub
  releases, disk space, and the daemon log below. Rerun `up` after fixing
  connectivity. Do not bypass a checksum failure; verify the release
  source and retry a fresh download.

## Permission denied, socket missing or connection refused

On the Linux root service, run:

```sh
sudo /usr/local/bin/boxpool ls
sudo systemctl status boxpool --no-pager
sudo journalctl -u boxpool -n 100 --no-pager
```

`/var/lib/boxpool` is root-owned mode `0750`; its socket is `root:root`
mode `0660`. An unprivileged permission error cannot tell you whether the
socket exists or the service is running. Do not start another daemon or
relax these permissions. The socket grants the daemon's privileges,
effectively root on this host. There is no Boxpool group.

For a missing/refused socket under the correct account, run `up` and read
the logs. Leave `BOXPOOL_HOME` unset for the Linux service and its clients.
If you previously used `login`, `logout` selects the local daemon again.

On a Mac:

```sh
launchctl print "gui/$(id -u)/run.boxpool.daemon"
tail -n 100 "$HOME/Library/Logs/boxpool/daemon.log"
boxpool up
```

Once the daemon responds, use `boxpool logs --daemon` (sudo on Linux).
Foreground runs also write `daemon.log` in the state directory.

## Join or a node is unreachable

1. On the head, confirm `ls --nodes` works locally and inspect
   `logs --daemon`. If another program owns TCP 7443, the daemon can be
   healthy locally while its pool listener is disabled; free the port and
   restart the service.
2. From the worker, check reachability with `nc -vz HEAD_IP 7443` if netcat
   is installed. Substitute the actual head IP. Allow TCP 7443 **between
   pool peers**, including replicas, in host firewalls and tailnet policy.
3. If a LAN hostname cannot resolve, create a fresh token on the head with
   `token create --join --addr HEAD_IP:7443`. For a tailnet, connect the
   system Tailscale client on both hosts and create with
   `--transport tailscale`. Userspace-only networking is unsupported.
4. If the token is used, expired, malformed, or names the old head after
   failover, generate a new token. Paste the entire value as one argument.
   Do not publish tokens in bug reports.
5. Wait for the next five-second heartbeat after joining or fixing
   connectivity. After 30 seconds without one, nodes and their sandboxes
   are `unreachable`; their disks and records have not been deleted.

## A sandbox will not start, has no network, or loses changes

- **Insufficient capacity:** stopped and paused sandboxes still reserve
  resources. Inspect `ls` and `ls --nodes`. Remove only unneeded sandboxes,
  choose another node, or request `--cpu 1 --mem 512m` for a new sandbox.
  Keep disk at least as large as the image. Admission leaves host capacity
  and conservatively charges full disk budgets against current free space.
- **No matching node:** `sh --node` takes a label, not a hostname. Set it
  locally on the worker with `node label worker`, then retry after a
  heartbeat. `--os macos` means a Mac host running a Linux guest.
- **Mac guest has no internet:** macOS 14–15 or a build with an older SDK
  has no sandbox NIC. Use macOS 26+ and the current signed release. Do not
  substitute shared NAT, which breaks sibling isolation.
- **Linux guest has no internet:** run the service as root with `ip` and
  `nft`. A foreground non-root Firecracker daemon deliberately has no
  sandbox network. Inspect daemon logs for setup failures.
- **Mac cannot allocate another network:** at most 32 sandboxes, including
  stopped ones and recently removed networks, can hold networks at once.
  Remove unneeded sandboxes and allow the two-minute quarantine to pass.
- **Docker changes outside `/root` disappear / disk limit seems ignored:**
  only `/root` persists, the image is read-only elsewhere, and the disk
  budget is advisory. Keep work in `/root`; see [persistence](guide.md#resources-and-persistence).
- **`exec` exits 125 / `sh` says stdin is not a terminal:** check daemon
  connectivity and sandbox state. Use an interactive terminal for `sh`,
  and `exec` for scripts. Flags go before the sandbox name.

## Preview or desktop fails

Run `boxpool expose check` **locally on the head** (sudo on Linux).
It checks connected Tailscale, MagicDNS and HTTPS certificates under the
actual daemon account. Creating a URL also checks Serve permissions.
Remote `login` does not redirect `expose` or `desktop` to the head.

A 502 mentioning the agent's `tcp` feature means the guest image is too
old. The default pinned `v0.0.1-alpha.1` image lacks it. A CLI update,
stop/start, or an old snapshot does not update that guest. See
[image rollout](expose.md#guest-images). A missing desktop rootfs/image
needs [explicit desktop setup](desktop.md#install-the-image-on-each-desktop-capable-node)
on the selected node. Install it before `sh --image desktop`.

For a URL that will not open, connect the viewing device to the same
tailnet and allow the allocated HTTPS port (not just 443). A 401 without
the secret link/cookie is expected. Open the whole printed link once.
For a desktop conflict, close the first viewer; only one can connect.
Use `desktop rm <sandbox>` before issuing a replacement link.

## Report a problem

Include OS/architecture, `boxpool version`, doctor output, the command
that failed, expected result and relevant daemon log lines in a
[new issue](https://github.com/Sukhendu2002/boxpool/issues/new/choose).
Remove tokens, secret URLs, cookies and private paths. Console logs do
not include normal shell/exec output; copy that output separately.
