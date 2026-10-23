# Base guest image

Issue #8: one Debian slim userland for Firecracker, Apple Virtualization
(`vz`) and Docker. No systemd, coding agents or desktop packages. The VM
rootfs is the **same Dockerfile userland stage** as the OCI image, including
the same statically linked `boxpool-agent` and `/etc/boxpool-image` version.

## Build

On Linux with Docker Engine, Docker buildx and Git, from a checkout:

```sh
image/build.sh amd64
# On a native Linux arm64 host:
image/build.sh arm64
```

No host compiler, Go installation, loop mounts, sudo or privileged container
is needed for the build. All compiler/filesystem tools run inside BuildKit.
Docker must be accessible to the invoking user. Select an existing working
engine with `DOCKER_CONTEXT` or `DOCKER_HOST` if needed; the script does not
change contexts, permissions or daemon settings. Docker Desktop on macOS
also works with a Linux engine. Cross-building uses GCC cross compilers for
the kernel and Go's cross compiler; running apt in the target userland
requires Docker binfmt/QEMU support when the target differs from the engine.
CI uses native runners for both architectures.

Allow about 10 GiB free in the Docker engine's storage and 1 GiB in the
output directory per retained build generation. The default is four compiler
jobs, with `JOBS=1` through `JOBS=8` supported to bound memory use. Builds can take several minutes.
Docker retains reusable layers; failed builds clean only their own incomplete
output directory. Complete generations are retained as described below. The
script never prunes Docker caches.

```sh
IMAGE_VERSION=v0.1.0 JOBS=4 image/build.sh amd64
```

The default version is `0.0.0-dev.<12-character-git-sha>`. Explicit versions
must use Docker tag characters (no `+`) and be at most 122 characters to
leave room for the `-<arch>` tag suffix; the same string goes into the agent's
`/v1/version`, `/etc/boxpool-image`, metadata and OCI tag. Uncommitted code
is included but the revision label is HEAD, so use a clean checkout when
publishing. Builds are repeatable from pinned inputs, **not claimed to be
bit-for-bit reproducible**: filesystem UUIDs/timestamps, image metadata and
compiler/build-engine details can vary.

For a source archive or the Linux development sync (which omits `.git`),
set `IMAGE_REVISION` to the full source commit SHA: 40 lowercase hexadecimal
characters, or 64 for a SHA-256 Git repository. This overrides checkout HEAD
when supplied and also determines the default version. The caller must
provide the actual source revision; no placeholder or guessed revision is
used. Input validation runs before Git or Docker checks. A source copy with
no revision fails with an actionable error even if `IMAGE_VERSION` is set.

```sh
# Capture this in the original checkout, then pass it to the source-copy build:
git rev-parse HEAD
IMAGE_REVISION=<full-source-commit-sha> image/build.sh amd64
```

Outputs are selected by `dist/image/<arch>/current/` (override the parent
with `OUTPUT_DIR`). Each build writes a unique hidden `.generation.*`
directory on that filesystem and saves the exact OCI image there. Once all
writes finish, a single atomic symlink rename selects the complete generation
as `current`. A failed build or failed switch leaves the prior selection
usable. Previous complete generations remain available, including candidates
retained when a switch fails or is interrupted. Nothing is broadly pruned.
A hard kill can leave an incomplete unselected directory; only `current`
identifies a published set. This protects against build errors and process
interruption, not filesystem corruption or power-loss durability.

The script prints `artifacts=<absolute-generation-path>`. For several reads,
pin that path or resolve `current` once with `cd .../current && pwd -P` to
avoid mixing generations if another build switches `current`. Retained
complete generations must not be modified while in use; decompress a copy
of the rootfs outside the generation. To reclaim space, explicitly remove
only a known unused generation after checking it is neither selected nor
being built or read.

Migration from the original flat output layout is non-destructive: existing
files under `OUTPUT_DIR` are left untouched, and all new consumers must use
`current` or the printed generation path. A real file/directory already named
`current` is rejected with an actionable error; move that entry aside
explicitly before building. New builds do not silently overwrite it.

Each selected generation contains:

- `vmlinux-amd64`: stripped uncompressed ELF, preferred for Firecracker.
- `bzImage-amd64`: x86 Linux boot-protocol image for Intel `VZLinuxBootLoader`;
  Firecracker also accepts it, with a decompression cost at boot.
- `vmlinux-arm64`: **uncompressed arm64 `arch/arm64/boot/Image`, with its
  EFI/PE header**, despite the shared artifact name. It is not an ELF file.
  Use it for Firecracker aarch64 and Apple silicon `VZLinuxBootLoader`.
- `rootfs-<arch>.ext4.zst`: compressed 512 MiB writable ext4 filesystem,
  including uid/gid, permissions, symlinks and hardlinks. No partition table
  and no initramfs; attach as the first virtio block disk (`/dev/vda`).
- `kernel-config-<arch>`, `kernel-<arch>.txt`, `image-<arch>.txt`: resolved
  kernel config, kernel source provenance, image version/revision.
- `SHA256SUMS-<arch>`: checksums for all VM assets above.
- `oci-<arch>.tar` and `.tar.sha256`: the exact loaded/tested OCI image, saved
  in Docker archive format for transferring between build and publish jobs.
  This archive is retained as a CI artifact, not a GitHub release asset.

The local OCI tag is `boxpool-image:<version>-<arch>`. Verify assets:

```sh
cd dist/image/amd64/current
sha256sum -c SHA256SUMS-amd64
sha256sum -c oci-amd64.tar.sha256
zstd -dc rootfs-amd64.ext4.zst > /tmp/boxpool-rootfs-amd64.ext4
file vmlinux-amd64 bzImage-amd64 /tmp/boxpool-rootfs-amd64.ext4
```

## Pins and kernel compatibility

New images include `procps` for `free -m`. The actual VM smoke boots with
1 vCPU and 512 MiB, checks `nproc`, and checks that `free -m` reports usable
guest RAM within the configured limit (the kernel reserves some RAM).
The currently pinned `v0.0.1-alpha.1` artifacts predate `procps`; read
`/proc/meminfo` there. Publishing a new image release is a separate step;
building these changes does not replace released images or their digest.

- Debian `bookworm-slim` multi-architecture index
  `sha256:7ff6ba31527b9e336162fa061746c366c4b2032f4d2cff2078bea43f78efe3f8`.
- Go `1.26.3-bookworm` index
  `sha256:386d475a660466863d9f8c766fec64d7fdad3edac2c6a05020c09534d71edb4b`.
- Debian and Debian security apt snapshots at `20261020T000000Z`. Release
  signatures and package hashes remain verified; only expired snapshot
  validity dates are ignored. No floating apt mirror or new Go dependency.
- Linux **6.18.54**, kernel.org tarball SHA-256
  `9df30b02dd8102bbd0be52556288ef6889ddbe7f1ddb96fbf847d0becf3eacac`, from
  kernel.org's [signed checksum list](https://cdn.kernel.org/pub/linux/kernel/v6.x/sha256sums.asc).
  Downloads must match this checked-in pin before extraction. The build
  verifies the hash, not a newly downloaded signing key/signature.
- Vendored, unmodified Firecracker `v1.17.0` 6.18 configs at commit
  `95f868c8e345b1cc8faccd1a3c910b4989dc3f58`, from
  [`resources/guest_configs`](https://github.com/firecracker-microvm/firecracker/tree/95f868c8e345b1cc8faccd1a3c910b4989dc3f58/resources/guest_configs).
  `amd64.config` SHA-256: `ba22401a0c7292a4c024ebcd10a562d4a1f1bfd2faed671406d3b159c0cf5215`;
  `arm64.config`: `35cce8e8b754a84523ca20dc068fc3f8f03a15523be36a8623bad54e11972e2a`.
  The upstream license is alongside the configs.

`build-kernel.sh` adapts those configs to the pinned upstream kernel with
`olddefconfig`, disables loadable modules and i8042 probing, removes debug
information, and requires built-in ext4, devtmpfs (including
`CONFIG_DEVTMPFS_MOUNT=y`), tmpfs, PTYs, proc/sysfs,
virtio block/network/console, MMIO, PCI and vsock support. Both use 4 KiB
pages. Config changes must be checked against both native builds and boots.
The [Firecracker kernel policy](https://github.com/firecracker-microvm/firecracker/blob/main/docs/kernel-policy.md)
warns that its configs target Amazon Linux microvm kernels; this adapted
kernel.org build is a boxpool image, **not an upstream-certified kernel**.
CI requires the focused amd64 KVM smoke below to validate it. Native arm64 KVM and
`vz` boot validation remain manual follow-ups for their backend work.

Format decisions follow Firecracker's
[kernel setup documentation](https://github.com/firecracker-microvm/firecracker/blob/main/docs/rootfs-and-kernel-setup.md)
and Apple's [Linux VM configuration](https://developer.apple.com/documentation/virtualization/creating-and-running-a-linux-virtual-machine).
For `vz`, use the matching CPU image, `VZLinuxBootLoader`, virtio block,
virtio socket and virtio serial console (`console=hvc0`), with
`root=/dev/vda rw init=/sbin/init`. All boot-critical drivers are built in,
so no initial RAM disk is needed. The driver itself is outside this issue.

## Init and container behavior

The VM kernel automatically mounts devtmpfs at `/dev` before running
`/sbin/init` (no initramfs; do not disable it with `devtmpfs.mount=0`). Init
mounts proc, verifies `/dev` is devtmpfs, and remounts it with `nosuid`.
A missing/wrong `/dev` mount or any mount failure stops init. It then mounts
sysfs, writable tmpfs `/run` and `/tmp`, shared memory and a new devpts
instance with
`/dev/ptmx` linked to `/dev/pts/ptmx`. After mounting proc, it creates
`/dev/fd -> /proc/self/fd` and `/dev/stdin`, `/dev/stdout`, `/dev/stderr`
links to descriptors 0, 1, 2 there. `/var/tmp` is writable on ext4. It
then **execs Debian tini as PID 1**, with the agent in its child process
group listening on vsock port **1024**. Tini reaps adopted orphans and
forwards signals to the process group. If the agent dies, tini exits;
the VM fails fast (`panic=1`) rather than leaving an unresponsive guest.
There is no shell/getty service. Network/DNS provisioning belongs to the
backend drivers; the vsock smoke needs neither a TAP device nor SSH.

Docker runs tini directly, using Docker's mounts and the agent's
`unix:/run/boxpool/agent.sock` transport. It needs no privileged mode or
extra capabilities. The Docker driver (`internal/backend/docker`) bind-mounts
`/run/boxpool` and runs the agent with all capabilities dropped.
The full command can be replaced without invoking VM initialization:

```sh
docker run --rm boxpool-image:v0.1.0-amd64 cat /etc/boxpool-image
docker run --rm boxpool-image:v0.1.0-amd64 \
  /usr/local/bin/boxpool-agent --listen unix:/run/boxpool/custom.sock
image/test-oci.sh boxpool-image:v0.1.0-amd64 v0.1.0
```

## Firecracker acceptance check

On Linux with readable/writable `/dev/kvm`, Go 1.24+, Python 3, bash,
curl, tar, coreutils, zstd and the built artifacts:

```sh
image/smoke.sh dist/image/amd64/current v0.1.0
```

It checks asset hashes, fetches the **same pinned/checksummed Firecracker
v1.17.0** as [`bench-boot.sh`](../scripts/bench-boot.sh), decompresses a
temporary disk, boots a 2-vCPU/256-MiB VM with PCI virtio and a vsock device
(CID 3), then calls `/v1/version` through the host-side vsock UDS. It checks
the exact protocol/agent/image version, PTY exec, proc TCP discovery,
mounts (including exactly one devtmpfs at `/dev` with `nosuid`), writable
directories, metadata and PID 1 orphan reaping. It also
pipes input through `/dev/stdin` and `/dev/fd/0` and checks writes through
`/dev/stdout` and `/dev/stderr` reach their respective redirected streams.
Success prints `GET /v1/version: protocol=1 agent=v0.1.0` and the smoke result.
Failure prints console/Firecracker tails. Boot/probe/shutdown are bounded;
the VM and temporary disk are removed on exit, keeping downloaded caches.
An arm64 host uses the same command with its arm64 artifact directory.

For a manual API boot, decompress/copy a disposable disk, start the pinned
Firecracker with `--api-sock /tmp/fc.sock --enable-pci`, then PUT these
objects using `curl --unix-socket /tmp/fc.sock -X PUT -H
'Content-Type: application/json' -d '<object>' http://localhost/<path>`:

1. `machine-config`: `{"vcpu_count":2,"mem_size_mib":256}`.
2. `boot-source`: `{"kernel_image_path":"/absolute/vmlinux-amd64","boot_args":"console=ttyS0 reboot=k panic=1 root=/dev/vda rw init=/sbin/init i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd"}`.
3. `drives/rootfs`: `{"drive_id":"rootfs","path_on_host":"/absolute/disposable.ext4","is_root_device":true,"is_read_only":false}`.
4. `vsock`: `{"guest_cid":3,"uds_path":"/tmp/guest.vsock"}`.
5. `actions`: `{"action_type":"InstanceStart"}`.
6. `go run ./image/smoke -socket /tmp/guest.vsock -version v0.1.0`.
7. Terminate that Firecracker PID, wait for it, remove its sockets and
   disposable disk. Never boot a shared writable base image concurrently.

The i8042 args preserve the
[boot spike's workaround](../docs/benchmarks/m1-boot.md), even though this
kernel disables that controller. Use `keep_bootcon console=ttyS0` for the
Firecracker arm64 serial console if diagnosing early boot.

## CI and release

The **Guest image** workflow builds amd64 on `ubuntu-24.04` and arm64 on
`ubuntu-24.04-arm`, tests both OCI images, checks hashes, and boots the
actual amd64 kernel/rootfs with KVM. Missing KVM fails, never silently
skips. The summary records the compressed rootfs byte count and hashes.
Per-architecture assets, including the saved OCI image, are uploaded for
seven days. PR/main builds and manual dispatch **never publish**.

Publishing happens only for a **published GitHub release** whose tag
matches `v<major>.<minor>.<patch>[-suffix]` and points into `main`'s history.
Both architectures are built/tested at that release commit and version in
the same run. Only the dependent publish job receives `contents:write` and
`packages:write`; no `pull_request_target`, PR-code credentials or package
push in build jobs. The publish job verifies checksums/version, loads the
saved images (no rebuild), pushes `:<version>-amd64` and `:<version>-arm64`,
creates `ghcr.io/sukhendu2002/boxpool-image:<version>` as a two-architecture
manifest, and uploads the VM assets/config/provenance plus per-architecture
and combined `SHA256SUMS` files to that release. No floating `latest` tag.

After publishing (not during PR validation):

```sh
docker run --rm ghcr.io/sukhendu2002/boxpool-image:v0.1.0 cat /etc/boxpool-image
# Expected: v0.1.0
gh release download v0.1.0 --pattern 'SHA256SUMS' --pattern 'vmlinux-*' \
  --pattern 'bzImage-*' --pattern 'rootfs-*' --pattern 'kernel-*' --pattern 'image-*'
sha256sum -c SHA256SUMS
```

A partially failed publication may leave versioned architecture tags or
some release assets; the workflow does not silently overwrite assets.
Inspect them before retrying. For rollback, keep consumers on the prior
explicit version. Nothing in this change downloads artifacts for users or
implements a backend driver or the general integration harness.
