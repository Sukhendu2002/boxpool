# M3 gate: resume, daemon crash recovery and disks across reboots

Issue #33. Measured on 2026-10-29 with `main` at `5871706` built from
source on each host, plus [`scripts/bench-m3.py`](../../scripts/bench-m3.py)
from this PR. The same script ran unchanged on both hosts.

**Status: PENDING.** Resume is under 1 s on both platforms, and kill -9
recovery passes on both. The Linux reference host kept the disk on
10 of 10 reboots. The Mac reboot runs have not been done yet:
this session ran on that Mac, so it could not reboot it.

## Acceptance criteria

| Criterion | Linux, Firecracker | macOS, vz | Result |
| --- | --- | --- | --- |
| Resume under 1 s | 78 ms median, 88 ms p90 | 528 ms median, 650 ms p90 | PASS |
| 10 of 10 reboot runs keep the disk | 10 of 10 | not run yet | PASS on Linux, PENDING on macOS |
| Daemon kill -9 recovery | 3 of 3 kept running, memory intact | 3 of 3 booted again, disk intact | PASS |
| Results committed | this document | this document | PASS |

## Machines

The Linux reference host is `sukhendu-pc`, the same as for the M1 and M2
gates: a bare-metal Intel Core i5-1235U, 12 threads, 19.2 GiB RAM, NVMe,
Ubuntu 24.04.3, kernel `7.0.0-38-generic`, `go1.27.3`. `systemd-detect-virt`
reported `none`. `sudo ./bin/boxpool up` installed the root systemd service
over the existing, empty `/var/lib/boxpool`, which was backed up first.

The Mac is an Apple M2 MacBook Air, 8 cores, 16 GiB RAM, on AC power,
macOS 27.0.1 (`26A434`). The binary was built and signed with
`scripts/dev-sign.sh`, and `boxpool up` installed it as the launch agent
with `BOXPOOL_HOME=~/.boxpool-m3-gate`. The state must be persistent:
a home under `/tmp` would not survive the reboot runs.

Both hosts ran the guest image and Firecracker pinned at `5871706`, with
the warm base snapshot from #32 in place.

## Method

`bench-m3.py` names its sandboxes `m3-<random>-*`, removes only those,
and exits nonzero on the first failed check. Each sandbox gets two tokens:
one in `/root/m3-token` on its disk, followed by `sync`, and one in
`/dev/shm/m3-token`, which lives only in guest RAM. The disk token shows
the disk survived. The RAM token shows whether the VM's memory survived:
after a resume it must still be there; after a reboot it must be gone.

- **resume**: one default-size sandbox (2 vCPU, 2 GiB, 10 GiB). Ten
  times: write tokens, `pause`, check `ls` says `paused`, then time
  `resume` and the first `exec` that reads the RAM token back. The
  reported number is the time until that `exec` answered, so it includes
  the CLI, the daemon and the agent's reconnection.
- **crash**: three sandboxes created with `--restart always` and 512 MiB,
  because three default 2 GiB VMs do not fit the 16 GiB Mac's host
  reserve. `kill -9` the daemon's PID as reported by systemd or launchd,
  wait for the service manager to start a new daemon and for it to answer,
  then check each sandbox is `running` and still has its disk token.
- **reboot-prepare / reboot-check**: prepare creates one default-size
  sandbox with `--restart always` and one without, writes tokens, and
  records the host's boot ID (`/proc/sys/kernel/random/boot_id`, or
  `kern.boottime` on macOS). Check refuses to run until the boot ID has
  changed. It then requires the `always` sandbox to come back `running`
  and the other to be `stopped`, starts that one, and requires both to
  have the disk token and to have lost the RAM token. Both are removed.

Median is the middle of the sorted samples (mean of the middle two); p90 is
nearest rank, the ninth of ten. All samples are shown; none was discarded.

## Resume

Milliseconds from `boxpool resume` until `exec` read the RAM token back.
The `resume` command alone took 71 ms median on Linux and 501 ms on the Mac.

| Run | Linux, Firecracker | Mac M2, vz |
| --- | ---: | ---: |
| 1 | 73 | 549 |
| 2 | 87 | 650 |
| 3 | 75 | 526 |
| 4 | 81 | 521 |
| 5 | 74 | 507 |
| 6 | 88 | 529 |
| 7 | 92 | 538 |
| 8 | 72 | 527 |
| 9 | 75 | 509 |
| 10 | 81 | 678 |
| Median | **78** | **528** |
| p90 | **88** | **650** |

Every resume kept the guest's memory: the RAM token written before the pause
was read back after it. The Mac's three-run integration benchmark in
[m3-macos-pause.md](m3-macos-pause.md) measured 222–230 ms to resume the VM
directly in the driver. The extra time here is the daemon path and CLI.

## Daemon kill -9

| | Linux | macOS |
| --- | --- | --- |
| Service manager | systemd, `Restart=on-failure` | launchd, `KeepAlive` on failure |
| New daemon answering after | 2.2 s | 2.7 s |
| Sandboxes `running` afterwards | 3 of 3 | 3 of 3 |
| Disk token | 3 of 3 | 3 of 3 |
| RAM token | 3 of 3 kept | 3 of 3 lost |

This difference is by design, as recorded in #27 and the `vz` package doc.
Firecracker VMs are separate processes that outlive the daemon, and the new
daemon adopts them, so their memory is kept. Virtualization framework VMs run
inside the daemon's process and die with it. launchd starts a new daemon, and
reconcile boots the `restart: always` sandboxes again from their disks. A
Mac sandbox without that policy is `stopped` after a daemon crash and needs
`boxpool start`.

## Reboots

### Linux

The runs were driven over SSH from the Mac: `reboot-prepare`,
`sudo systemctl reboot`, wait until SSH answers with a new boot ID,
`reboot-check`.

| Run | Host back after | `always` sandbox | other sandbox | Disk tokens | RAM tokens | Result |
| --- | ---: | --- | --- | --- | --- | --- |
| 1 | 36 s | running | stopped, started | 2 of 2 | 2 of 2 lost | PASS |
| 2 | 35 s | running | stopped, started | 2 of 2 | 2 of 2 lost | PASS |
| 3 | 35 s | running | stopped, started | 2 of 2 | 2 of 2 lost | PASS |
| 4 | 36 s | running | stopped, started | 2 of 2 | 2 of 2 lost | PASS |
| 5 | 56 s | running | stopped, started | 2 of 2 | 2 of 2 lost | PASS |
| 6 | 35 s | running | stopped, started | 2 of 2 | 2 of 2 lost | PASS |
| 7 | 35 s | running | stopped, started | 2 of 2 | 2 of 2 lost | PASS |
| 8 | 35 s | running | stopped, started | 2 of 2 | 2 of 2 lost | PASS |
| 9 | 35 s | running | stopped, started | 2 of 2 | 2 of 2 lost | PASS |
| 10 | 36 s | running | stopped, started | 2 of 2 | 2 of 2 lost | PASS |

No Firecracker processes, `bp-*` TAPs or `m3-*` sandboxes remained afterwards.

### macOS

Not run yet. Each run, from the repository root on the Mac:

```sh
export BOXPOOL_HOME=~/.boxpool-m3-gate
bp="$BOXPOOL_HOME/bin/boxpool"
python3 scripts/bench-m3.py "$bp" reboot-prepare "$BOXPOOL_HOME/m3-reboot.json"
sudo shutdown -r now
# after logging in again; the launch agent starts the daemon at login
python3 scripts/bench-m3.py "$bp" reboot-check "$BOXPOOL_HOME/m3-reboot.json"
```

Repeat ten times and add each run's JSON line here.

## Reproduce

On Linux, with no other boxpool installation, from the repository root:

```sh
go build -o bin/boxpool ./cmd/boxpool
sudo ./bin/boxpool up
sudo python3 scripts/bench-m3.py /usr/local/bin/boxpool resume --runs 10
sudo python3 scripts/bench-m3.py /usr/local/bin/boxpool crash
```

On a Mac, with no `run.boxpool.daemon` launch agent installed:

```sh
go build -o bin/boxpool ./cmd/boxpool && scripts/dev-sign.sh bin/boxpool
export BOXPOOL_HOME=~/.boxpool-m3-gate
bin/boxpool up
python3 scripts/bench-m3.py "$BOXPOOL_HOME/bin/boxpool" resume --runs 10
python3 scripts/bench-m3.py "$BOXPOOL_HOME/bin/boxpool" crash
```

For the reboot runs, use `reboot-prepare` and `reboot-check` as above. On
Linux, keep the state file out of `/tmp`, which is cleared at boot.

The script's own checks run in CI with the other harness tests:

```sh
python3 -m unittest discover -s scripts -p 'test_*.py'
```

`test_bench_m3.py` runs `reboot-check` against a stub boxpool. It must fail
when a disk token is lost, when a sandbox without `restart: always` comes
back running, and when the host has not rebooted. In that last case it must
also leave the sandboxes and the state file in place.

## Decision

PENDING the Mac reboot runs. Resume and crash recovery meet the gate on both
platforms. Do not start M4 until the Mac reboot rows read 10 of 10.
