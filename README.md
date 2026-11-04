# Boxpool

Turn your spare Linux machines and Apple silicon Macs into a pool of
sandboxes for AI agents. Run commands in Linux VMs on hardware you own,
add machines to the pool, and open sandbox apps from your phone.

Early releases are available; Boxpool is under active development. Linux uses Firecracker with KVM, or a
Docker container fallback with weaker isolation. Macs use Apple's
Virtualization framework. Read the [trust model](docs/security.md#trust-at-a-glance)
before running untrusted workloads.

## Quickstart: a two-machine pool

Choose two machines you control. **Head** is the first machine; it
coordinates the pool. **Worker** is the second; it contributes capacity.
You need a terminal on each (SSH is fine) and internet for the first
image download. Allow about ten minutes once host prerequisites and
network access are ready; first downloads may take longer.

Both hosts need at least **20 GB free** in the state filesystem, plus CPU
and memory for sandboxes. Defaults per sandbox are 2 CPUs, 2 GiB RAM and
10 GiB disk; Boxpool also reserves one CPU and at least 1 GiB RAM for the
host. The smaller examples below request 1 CPU and 512 MiB RAM.

| Host | Requirements |
| --- | --- |
| Linux amd64/arm64 | Kernel 5.10+, systemd, root access; `/dev/kvm` for Firecracker; `e2fsprogs`, `ip` and `nft` |
| Linux without KVM | Local rootful Docker Engine running, plus `ip` and `nft`; containers share the host kernel |
| Apple silicon Mac | macOS 14+, APFS; **macOS 26+ for sandbox internet access**; run Boxpool as your user |

Intel Macs and Windows hosts are unsupported. Guests are Linux, including
on a Mac; `--os macos` selects a Mac **host**.

### 1. Install on both machines

On Debian/Ubuntu hosts, install the networking and disk tools first:

```sh
sudo apt-get update
sudo apt-get install -y curl e2fsprogs iproute2 nftables
```

On Fedora, use `sudo dnf install curl e2fsprogs iproute nftables`.
For Docker fallback, also install and start [Docker Engine](https://docs.docker.com/engine/install/).
Then on **both** machines:

```sh
curl -fsSL https://boxpool.run/install | sh
boxpool version
```

The installer verifies the release's SHA-256 checksum and never runs sudo
or starts a service. If it installs to `~/.local/bin`, add it to your
current shell's path with `export PATH="$HOME/.local/bin:$PATH"` and add
that line to your shell startup file for later sessions.

Alternatively, use `brew install sukhendu2002/tap/boxpool`, or with Node.js
20+, `npm install -g boxpool`. `npx boxpool doctor` can check a host
without a global npm install; use an installed binary for Linux service
setup, and do not run `sudo npx`. See [install options and source builds](docs/guide.md#installation-options).
The Mac release is ad-hoc signed and not notarized; if macOS blocks it,
follow the [per-file verification instructions](docs/development.md#releases).

### 2. Start the head

On a **Linux head** (use the path printed by the installer for the first command):

```sh
sudo "$(command -v boxpool)" doctor
sudo "$(command -v boxpool)" up
sudo /usr/local/bin/boxpool ls --nodes
```

On a **Mac head**:

```sh
boxpool doctor
boxpool up
boxpool ls --nodes
```

`doctor` should exit successfully and print `Backend: firecracker`,
`Backend: docker` (with an isolation warning), or
`Backend: virtualization framework`. Not every check must be green: Docker
is optional when KVM works, and KVM is optional for Docker fallback.
See [troubleshooting by check name](docs/troubleshooting.md) if it fails.

`up` installs and starts the daemon, downloading the pinned VM image on
first start. `ls --nodes` should now show one `ready` head. On Linux, keep
using `sudo /usr/local/bin/boxpool` for **all local daemon commands**;
on macOS use `boxpool` without sudo.

### 3. Join the worker

The worker must reach the head on TCP **7443**. Replicas also need to
accept connections from their pool peers on that port; allow it between
both machines in host firewalls. For this LAN example, choose the head's
LAN IP address reachable from the worker. On the **head**, replace the
example IP below with that address:

```sh
# Linux head
sudo /usr/local/bin/boxpool token create --join --addr 192.168.1.10:7443
# Mac head: boxpool token create --join --addr 192.168.1.10:7443
```

This prints a secret beginning with `bpjoin`. Copy the **whole token**.
It is single-use and expires after one hour by default.

On the **worker**, replace `PASTE_JOIN_TOKEN` with that token:

```sh
# Linux worker
sudo "$(command -v boxpool)" doctor
sudo "$(command -v boxpool)" join 'PASTE_JOIN_TOKEN'
sudo /usr/local/bin/boxpool node label worker
# Mac worker: boxpool doctor
#             boxpool join 'PASTE_JOIN_TOKEN'
#             boxpool node label worker
```

`join` starts the worker's daemon itself; expect `joined the pool at ... as
node ...`. Labels are comma-separated selectors; `worker` here is a label
you set, not a special role. After about five seconds, on the **head**:

```sh
# Linux head
sudo /usr/local/bin/boxpool ls --nodes
# Mac head: boxpool ls --nodes
```

Expect **two ready nodes**. The second may have role `replica`; it still
runs sandboxes. If the head is unreachable or a token expires, see
[join troubleshooting](docs/troubleshooting.md#join-or-a-node-is-unreachable).

**Different networks:** first connect both machines to the same Tailscale
or Headscale tailnet with the system Tailscale client. On the head, replace
the token command with `boxpool token create --join --transport tailscale`
(with sudo on Linux). Join as above. Allow TCP 7443 between pool peers in
tailnet policy and host firewalls. No router port forwarding is needed.
Userspace-only Tailscale networking is unsupported.

### 4. Run a sandbox on the worker

On the **head**, open an interactive terminal and run:

```sh
# Linux head
sudo /usr/local/bin/boxpool sh --node worker --name hello --cpu 1 --mem 512m
# Mac head: boxpool sh --node worker --name hello --cpu 1 --mem 512m
```

At the sandbox's `root@...` prompt, run:

```sh
uname -s
printf 'hello from a sandbox\n' > /root/hello.txt
cat /root/hello.txt
exit
```

Expect `Linux` and `hello from a sandbox`. Back on the head:

```sh
# Linux head
sudo /usr/local/bin/boxpool exec hello -- cat /root/hello.txt
sudo /usr/local/bin/boxpool ls
# Mac head: boxpool exec hello -- cat /root/hello.txt
#           boxpool ls
```

Expect the same text and `hello` in state `running`, owned by the worker.
Exiting a shell leaves the sandbox running. Reconnect with `boxpool sh hello`
(with sudo on Linux). You now have a two-node pool.

### 5. Open an app or desktop (optional)

App previews and desktops need Tailscale on the head and viewing device,
MagicDNS and HTTPS certificates enabled, and a guest image with the new
agent. **The default pinned `v0.0.1-alpha.1` image predates these features.**
Updating the CLI does not update existing sandbox images.

Follow [app preview and image setup](docs/expose.md), then start a server
inside a sandbox and run `boxpool expose <sandbox> 8000` on the head.
Open its secret HTTPS link on a tailnet-connected phone or laptop.
`boxpool expose check` checks the head's Tailscale prerequisites.

For a graphical browser, first follow [desktop image installation](docs/desktop.md),
then run on the head:

```sh
boxpool sh --image desktop --name browser
# Exit the shell, then:
boxpool desktop --no-open browser
```

Use sudo and `/usr/local/bin/boxpool` on Linux. Open the printed link;
the viewer starts read-only. **Take over** enables keyboard/mouse and
**Release control** returns to watching. Keep links private; their holders
can access the app or take over the desktop.

### 6. Clean up the example

On the **head**, remove only the example sandbox when finished:

```sh
# Linux head
sudo /usr/local/bin/boxpool stop hello
sudo /usr/local/bin/boxpool rm hello
# Mac head: boxpool stop hello
#           boxpool rm hello
```

`rm` deletes its disk. To keep work, stop it and start it later instead.
To stop a node's daemon, run `sudo /usr/local/bin/boxpool down` on Linux
or `boxpool down` on macOS. Linux VMs/containers keep running after `down`;
Mac VMs stop. Stop sandboxes explicitly first if you want them all stopped.
State is kept, and `up` starts the service again.

## Documentation

- [User guide](docs/guide.md): files, resources, snapshots, remote login,
  scheduling, failover, state and upgrades.
- [Command reference](docs/commands.md): generated from CLI help.
- [Troubleshooting](docs/troubleshooting.md): doctor, startup, join, images
  and logs.
- [App previews](docs/expose.md) and [desktop takeover](docs/desktop.md).
- [Security and trust model](docs/security.md).
- [Development](docs/development.md), [guest images](image/README.md),
  [agent protocol](docs/agent-protocol.md), and [distribution maintenance](docs/distribution.md).

Contributors: `go build -o bin/boxpool ./cmd/boxpool`, `go vet ./...`,
`go test ./...`. See [AGENTS.md](AGENTS.md) for contribution rules and the
[onboarding walkthrough](docs/onboarding-check.md) for the timed user test.

Apache 2.0. See [LICENSE](LICENSE).
