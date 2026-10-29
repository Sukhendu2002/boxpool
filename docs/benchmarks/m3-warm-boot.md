# M3 warm base boot (#32)

## Implementation

On daemon startup (including `up`), Firecracker and Virtualization.framework
boot and pause a default-size base VM before serving health. Its disk and saved
memory live under `warm/<backend>/sandboxes/base` in the state directory,
outside user sandbox listings and reservations. Once built it consumes no
CPU or RAM. Disk and memory files remain on disk; clones share blocks where
the filesystem supports it, otherwise Linux preserves sparse holes.

The cache key includes the published artifact contents, default resource
sizes and networking availability; macOS also includes the OS build. An
image upgrade rebuilds it on the next daemon start. A failed build is
cleaned up and logged, and the daemon serves the existing cold-boot path.

New default-size sandboxes clone both disk and memory. Firecracker overrides
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
- Warm: 608, 569, 562, 548, 537, 577, 549, 541, 537, 540.
  Median: **548.5 ms**.

**The reference Linux median-under-1-second acceptance criterion is still
unverified.** No reference Linux host was configured in this session. Hosted
KVM tests are correctness evidence, not reference-hardware performance.
Repeat the ten-shell procedure on the reference i5-1235U Linux box before
closing the performance acceptance criterion.

To discard a cache, stop the daemon, remove only `warm/` in its state
directory, and start it again. Existing sandbox disks are unaffected.
