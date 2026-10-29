# M3 warm base boot (#32)

## Implementation

On daemon startup (including `up`), Firecracker and Virtualization.framework
boot and pause a default-size base VM before serving health. Its disk and saved
memory live under `warm/<backend>/sandboxes/base` in the state directory,
outside user sandbox listings and reservations. Once built it consumes no
CPU or RAM. Disk and memory files remain on disk. Private disks share blocks
where the filesystem supports it; otherwise Linux copies sparse data. On
Firecracker, hard links share the immutable RAM file without copying it
(falling back to a copy when linking is unavailable). Each VM's RAM writes
remain private through Firecracker's `MAP_PRIVATE` mapping.

The cache key includes the published artifact contents, default resource
sizes and networking availability; macOS also includes the OS build. An
image upgrade rebuilds it on the next daemon start. A failed build is
cleaned up and logged, and the daemon serves the existing cold-boot path.

The base filesystem is flushed and frozen before saving memory, so in-flight
disk writes cannot race the snapshot or its later clone. Each fresh restore
thaws its private filesystem before resizing or writing to it. A durable
readiness marker keeps concurrent exec/file requests out until identity
initialization finishes; Firecracker adoption kills unfinished fresh VMs.

New default-size sandboxes clone their disk and saved state. Firecracker
[loads immutable snapshot RAM on demand with copy-on-write](https://github.com/firecracker-microvm/firecracker/blob/v1.17.0/docs/snapshotting/snapshot-support.md),
so sharing its inode never shares guest writes. Firecracker overrides
the template's TAP and vsock paths and redirects its root drive before resuming
vCPUs. macOS retains the virtual device identifiers required by saved state,
but attaches each VM to its own vmnet network and replaces the captured guest
address, route and resolver with that network's values. Guests get distinct
hostnames and `/etc/machine-id`, and restored kernel RNGs are explicitly
reseeded from host entropy before user commands can run. Existing sandboxes
keep their identity and disk on restart. No guest-image release is needed:
the pinned image already includes Perl and the required Linux ioctls.

Only the default **2 CPUs / 2048 MiB / 10 GiB** configuration uses this cache.
Custom resource sizes and named disk snapshots still cold-boot: their disk
and VM configuration cannot safely be mixed with the base's saved memory.
Docker is unchanged. Cache preparation on unsupported save/restore hosts
falls back to cold boot with a daemon warning.

## Automated checks

```sh
go test ./...
go test -race ./...
go vet ./...
go build -o bin/boxpool ./cmd/boxpool
```

Unit tests cover cache reuse and invalidation, failed preparation cleanup,
configuration eligibility, private clones and failed-clone reservation
cleanup, guest identity initialization/preservation, and warm-before-health
with cold-boot fallback.

Linux KVM CI runs `TestIntegrationWarm` both unprivileged (no network) and
as root (TAP/NAT networking), alongside existing boot/pause/network tests.
The shared integration check verifies saved-state use, private disks,
hostnames, machine IDs, networking/DNS, pause/resume and cold restart.
Four-CPU hosted runners test one clone to respect production admission;
the simultaneous two-clone identity/address check needs at least five CPUs.

On a real Mac, sign the integration binary:

```sh
go test -c -tags integration -o /tmp/vz.test ./internal/backend/vz
scripts/dev-sign.sh /tmp/vz.test
/tmp/vz.test -test.v -test.run '^TestIntegration(Warm|Pause|Network|NoNetwork)?$' -test.timeout 8m
```

On 2026-10-29 this passed on an Apple M2 MacBook Air (8 CPUs, 16 GiB),
macOS 27 arm64. The warm-clone check observed different hostnames, machine
IDs and IPv4 addresses, working DNS both initially and after pause/resume,
and persistent private files and identity after cold restart. The existing
network isolation and no-network tests also passed.

## Ten-shell timing

Use a real terminal, an already healthy daemon, and remove each sandbox
after its sample so reservations do not accumulate:

```sh
prefix="warm-bench-$(date +%s)-$$"
for i in $(seq 1 10); do
    boxpool sh --name "$prefix-$i" --timing -- printf 'ready\n'
    boxpool rm --force "$prefix-$i"
done
```

`-- true` produces no output, so the current `--timing` (time to first
output) prints nothing; `printf` measures the intended path. Each sample
creates a new sandbox, not a reattach to an existing one. On Linux run these
commands with access to the root daemon's socket.

Local macOS measurement on the machine above, using separate scratch state
directories, the same cached artifacts and signed binaries. `sh` ran under
`script` with a 24x80 PTY. Numbers are time to first output, in milliseconds;
base preparation and removal are outside each sample:

- Baseline `main` (`f7bb63f`): 838, 611, 606, 606, 581, 558, 600, 592, 576, 562.
  Median: **596 ms**.
- Warm: 579, 568, 552, 541, 535, 653, 586, 577, 552, 494.
  Median: **560 ms**.

## Reference Linux results (2026-10-29)

**The reference Linux median-under-1-second criterion passed: 124 ms.**

Machine: `sukhendu-pc`, bare-metal Intel Core i5-1235U (12 threads),
19.2 GiB RAM, Linux `7.0.0-38-generic` x86_64, ext4 on NVMe, AC power,
`powersave` governor. `systemd-detect-virt` reported `none`; the connecting
user could read/write `/dev/kvm`, and passwordless sudo was available.
Go was `go1.27.3 linux/amd64`; `doctor` selected Firecracker.

Source: baseline `f7bb63f`, final warm implementation `d11c59d`. Both used
the same verified artifact cache, default resources, independent scratch
daemons and a 24x80 PTY supplied by util-linux `script -q -e -c`. The installed
service was inactive and was not changed. Each of ten samples ran
`sh --name <fresh-name> --timing -- printf ready`, then removed that sandbox.
Startup/cache preparation and removal are outside the measured interval.

- Baseline cold samples (ms): 431, 418, 437, 401, 388, 412, 424, 400, 398, 402.
  Median: **407 ms**.
- Final warm samples (ms): 125, 111, 119, 127, 123, 132, 126, 120, 134, 114.
  Median: **124 ms**; range: **111–134 ms**.

The initial measurement on `65dbff7` had a 775 ms warm median: copying the
full 2 GiB RAM snapshot on ext4 outweighed the saved boot time. Sharing its
immutable inode removed this copy; a regression test checks that a private
memory mapping cannot modify the cached base.

Reference-host verification:

```sh
go vet ./...
go test ./...
go test -race ./...
go build -o bin/boxpool ./cmd/boxpool
go test -c -tags integration -o bin/fc.test ./internal/backend/firecracker
go test -c -tags integration -o bin/daemon.test ./internal/daemon
```

All passed. Initial real-backend checks also passed:
`TestIntegration`, `TestIntegrationPause`, `TestIntegrationNetwork`,
`TestIntegrationWarm` (unprivileged and root), and
`TestIntegrationSh/firecracker`.

After the shared-memory change, the unprivileged `TestIntegrationWarm`
passed again, and root `TestIntegrationWarm -test.count=3` passed: six
networked warm clones across three runs, with two simultaneous sandboxes
per run. The first run observed `10.200.0.6` and `10.200.0.10`; subsequent
runs used `10.200.0.2` and `10.200.0.6`. Machine IDs differed, as did hostnames;
private files and identity survived pause/resume and cold restart, and DNS
worked before and after resume. No Firecracker processes or Boxpool TAPs
remained after the checks and timing runs.

To discard a cache, stop the daemon, remove only `warm/` in its state
directory, and start it again. Existing sandbox disks are unaffected.
