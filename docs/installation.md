# Install Boxpool

Install the CLI on the machine that will run workspaces. A client laptop that
only connects to an existing pool needs the CLI, but does not need a hypervisor
or `boxpool up`; see [client setup](guide.md#use-the-pool-from-a-client-laptop).

## Host requirements

| Host | Requirements |
| --- | --- |
| Apple silicon Mac | macOS 14+, APFS; **macOS 26+ for workspace internet access**; run as your normal user |
| Linux amd64 / arm64 | Kernel 5.10+, systemd, root access; `/dev/kvm` for Firecracker; `e2fsprogs`, `ip` and `nft` |
| Linux without KVM | Local rootful Docker Engine running, plus `ip` and `nft`; containers share the host kernel |

Intel Macs and Windows are unsupported hosts. All guests are Linux.
Each compute host needs **20 GB available** in the state filesystem, plus CPU
and RAM for workspaces. On macOS, available space includes OS-reclaimable
capacity; `doctor` also reports immediately free space when it differs.
Boxpool reserves one CPU and at least 1 GiB RAM for the host. Plug laptops
into power for long-running work.

## Linux prerequisites

**On a Debian or Ubuntu host:**

```sh
sudo apt-get update
sudo apt-get install -y curl e2fsprogs iproute2 nftables
```

**On a Fedora host:**

```sh
sudo dnf install curl e2fsprogs iproute nftables
```

For Docker fallback, also install and start [Docker Engine](https://docs.docker.com/engine/install/).
Mac hosts do not need these Linux packages.

## Install the CLI

Choose one method. The shell installer works on supported Linux hosts and
Apple silicon Macs.

### Shell installer

**On the host:**

```sh
curl -fsSL https://boxpool.run/install | sh
boxpool version
```

The installer verifies the release SHA-256 checksum and never runs sudo or
starts a service. It chooses `/usr/local/bin` when writable, otherwise
`~/.local/bin`. If needed, add that directory to your current shell and your
shell startup file:

```sh
export PATH="$HOME/.local/bin:$PATH"
```

To review the installer first, choose a directory or pin a release:

```sh
curl -fsSL https://boxpool.run/install -o /tmp/boxpool-install.sh
# Inspect /tmp/boxpool-install.sh before running it.
BOXPOOL_INSTALL_DIR="$HOME/.local/bin" BOXPOOL_VERSION=v0.1.4 sh /tmp/boxpool-install.sh
```

### Homebrew

**On the host with Homebrew installed:**

```sh
brew install sukhendu2002/tap/boxpool
boxpool version
```

### npm

**On the host with Node.js 20+:**

```sh
npm install -g boxpool
boxpool version
```

To check a host without a global npm install, use `npx boxpool doctor`.
For a pinned one-off CLI, use `npx boxpool@0.1.4 version`.
The dependency-free launcher downloads on first use and verifies its pinned
digest, including cached binaries. `BOXPOOL_CACHE_DIR` changes its cache path.
Use an installed binary for Linux service setup; do not run `sudo npx`.

Homebrew pins SHA-256 and the shell installer checks the release manifest.
These checks detect corruption, but are not independent signatures if the
release account is compromised. Mac releases are **ad-hoc signed, not
notarized**. If macOS blocks a download, follow the
[per-file verification instructions](development.md#releases).

## Check your host

**On your Mac:**

```sh
boxpool doctor
```

**On your Linux host:**

```sh
sudo "$(command -v boxpool)" doctor
```

Expect a successful exit and `Backend: virtualization framework`,
`Backend: firecracker`, or `Backend: docker` with its isolation warning.
Not every optional check must be green: Docker is optional when KVM works,
and KVM is optional for Docker fallback. Follow
[troubleshooting by check name](troubleshooting.md) for failures.

Next: [start the service and create a workspace](first-workspace.md).

## Build from source

For unreleased features, use Git, Go 1.25+ and the host prerequisites above.
On a Mac, install Xcode Command Line Tools with `xcode-select --install`;
workspace networking also needs the macOS 26 SDK.

**On either host:**

```sh
git clone https://github.com/Sukhendu2002/boxpool.git
cd boxpool
go build -o bin/boxpool ./cmd/boxpool
```

**On your Mac**, sign with the virtualization entitlement before running:

```sh
scripts/dev-sign.sh bin/boxpool
./bin/boxpool doctor
./bin/boxpool up
```

**On your Linux host:**

```sh
sudo ./bin/boxpool doctor
sudo ./bin/boxpool up
```

On Mac, use `./bin/boxpool` for subsequent commands if you are testing the
source build. On Linux, `up` installs the binary at `/usr/local/bin/boxpool`;
use `sudo /usr/local/bin/boxpool` for subsequent local daemon commands.
See [contributing](../CONTRIBUTING.md) for development checks.
