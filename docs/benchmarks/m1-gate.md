# M1 gate: Linux single node

Issue #19. Checks the M1 exit criteria on real hardware.

**Status:** criteria 1 and 2 pass on the reference Linux box. Criterion 3 was
not run: no old laptop is available. It is tracked in #98, and M2 starts
without it (see [Decision](#decision)).

## Results

| # | Criterion | Target | Result | Status |
| --- | --- | --- | --- | --- |
| 1 | Install to first shell | under 5 min | 36.6 s and 41.0 s from a wiped state (1.3 s with cached artifacts) | **PASS** |
| 2 | `boxpool sh` cold boot, reference box | under 2 s median | **437 ms median, 448 ms p90** over 10 runs | **PASS** |
| 3 | Old laptop runs a sandbox; `doctor` explains the backend | runs | not run: no hardware available (#98) | **NOT RUN** |
| 4 | Results committed; misses get follow-ups and a decision | this doc | this doc, #98 | **PASS** |

All integration tests green on main, at `f556c29`:
[ci](https://github.com/Sukhendu2002/boxpool/actions/runs/39998370788),
[Linux KVM](https://github.com/Sukhendu2002/boxpool/actions/runs/39998370759)
(including the Firecracker integration job) and
[Docker fallback](https://github.com/Sukhendu2002/boxpool/actions/runs/39998370744).

## Machine

Run on 2026-10-23 on the reference box from [m1-boot.md](m1-boot.md), the
owner's bare-metal Linux laptop: Intel Core i5-1235U (10 cores, 12 threads),
19.2 GiB, 512 GB NVMe SSD, Ubuntu 24.04.3, host kernel `7.0.0-28-generic`,
`systemd-detect-virt`: `none`, on AC power. Boxpool commit `f556c29`
(`origin/main`), built on the box with Go 1.27.3, Firecracker backend, daemon
running as root under systemd.

The machine is not a fresh OS install. It had no boxpool on it: no
`/usr/local/bin/boxpool`, no `boxpool.service`, no `/var/lib/boxpool`, and no
`bp-*` links. Go, Docker and `e2fsprogs` were already installed. Afterwards it
was put back exactly: service, unit, binary and state removed, no
`firecracker` processes or `bp-*` links left, and `sudo nft list tables`
identical to before.

`doctor` on the box, as the regular user (who has `/dev/kvm` access but is
not in the `docker` group):

```text
boxpool doctor: linux/amd64

  ✓ Linux kernel                   7.0.0-28-generic
  ✓ VM host                        no VM indicators detected
  ✓ /dev/kvm                       present and accessible
  ✓ CPU virtualization (vmx/svm)   supported
  ✗ docker engine (fallback)       docker: permission denied on /var/run/docker.sock: run as root or as a member of the docker group (which is root-equivalent): Get "http://docker/version": dial unix /var/run/docker.sock: connect: permission denied
  ✓ resize2fs                      /usr/sbin/resize2fs
  ✓ free disk                      33 GB free at /var/lib/boxpool (need 20 GB)

Backend: firecracker
```

As root (`sudo boxpool doctor`), the Docker line reads
`✓ docker engine (fallback)  Docker 29.1.2, API 1.52, on /var/run/docker.sock`.
Everything else is the same.

## Method

[`scripts/bench-gate.sh`](../../scripts/bench-gate.sh) takes both timings.
It needs a Linux box with KVM, systemd and passwordless `sudo`.

`boxpool sh` takes no command (the issue's `boxpool sh --timing -- true`
does not exist), and it needs a terminal. So the script runs each shell
under `script(1)`, feeds it `exit`, reads the `time to prompt:` line, and
then removes the sandbox with `sudo boxpool rm --force`. `--timing` measures
from the start of `boxpool sh` to the first output of the sandbox's login
shell. For a new sandbox that includes the create request (rootfs copy and
resize), the Firecracker boot, the guest agent coming up, and `bash -l`
starting.

### 1. Install to first shell

The README has no step-by-step install section (see [Gaps found](#gaps-found)),
so this follows its `up` description and its Development build:

```sh
git clone https://github.com/Sukhendu2002/boxpool.git && cd boxpool
go build -o bin/boxpool ./cmd/boxpool
scripts/bench-gate.sh install ./bin/boxpool
```

The timer runs from `sudo ./bin/boxpool up` starting to the first shell
output of `sudo boxpool sh` in a new sandbox: `up` (doctor's checks,
installing the binary and unit, starting the service, the daemon downloading
Firecracker and the guest image) plus `sh`'s own `--timing`. It leaves out a
few ms of `script` and `sudo` startup between the two. The clone and build
are not timed: the build took a few seconds with Go already installed.

Between runs 1 and 2 everything was wiped: `sudo boxpool down`, the unit,
the `.wants` link and the binary removed, `systemctl daemon-reload`, and
`sudo rm -rf /var/lib/boxpool`. Run 3 removed the unit and binary but kept
`/var/lib/boxpool`, so its artifacts were already downloaded.

| Run | State before | `up` | `sh` to prompt | Total |
| --- | --- | --- | --- | --- |
| 1 | wiped (downloads artifacts) | 35,978 ms | 639 ms | **36.6 s** |
| 2 | wiped (downloads artifacts) | 40,595 ms | 407 ms | **41.0 s** |
| 3 | artifacts cached | 835 ms | 429 ms | 1.3 s |

Almost all of a first install is the download. The artifacts take 159 MB on
disk once unpacked. Download time depends on the network. Here, 36–41 s
leaves a lot of headroom under 5 minutes.

### 2. Cold boot, 10 runs

```sh
scripts/bench-gate.sh boot 10
```

Each run creates a new sandbox (`gate-1` … `gate-10`) with the defaults
(2 vCPU, 2 GiB, 10 GiB disk), so every boot is a cold create and boot of a
new microVM. Artifacts were cached and the host page cache was warm, as in
m1-boot.md. The daemon ran as root, so each sandbox also got its tap device
and firewall rules.

Time to prompt, in ms:

```text
 1 431
 2 417
 3 410
 4 432
 5 451
 6 439
 7 435
 8 448
 9 441
10 439
```

Median **437 ms** (the mean of the middle two, 435 and 439), p90 **448 ms**
(nearest rank: the 9th of 10 sorted values). The target is 2 s.

That beats the #2 spike on the same box, 1.07 s with the quickstart's boot
args and 0.57 s with the i8042 probe off
([m1-boot.md](m1-boot.md#results)). The comparison is not exact. The spike
measured from the Firecracker process start to Ubuntu's systemd autologin
prompt on the serial console. This measures the whole CLI path to the agent's
`bash -l`, on boxpool's own image. That image sets the i8042 boot args, and
its init is a short shell script, not systemd.

### 3. Old laptop

Not run: no hardware is available. The steps are in #98. It should record the
hardware, whether `/dev/kvm` exists, `doctor`'s backend and reason, the
install and boot timings, and the old-laptop row in m1-boot.md.

## Decision

**Proceed to M2.** Criteria 1, 2 and 4 pass, and criterion 2 passes with
more than 4x headroom. Criterion 3 is not passed. It is deferred to #98 (P1,
milestone M2), not dropped, for these reasons:

- The only blocker is hardware. No code is known to be missing, and nothing
  in M2 (macOS) depends on the old-laptop result.
- The risk it retires is old or nested hardware with no usable KVM. CI
  partly covers that today: the
  [Docker fallback](https://github.com/Sukhendu2002/boxpool/actions/runs/39998370744)
  workflow runs the fallback on every PR. The Linux KVM workflow boots
  Firecracker on nested KVM. `doctor`'s backend choice and its warning are
  unit-tested.
- What is still unknown is how a real old CPU and its firmware behave: no
  VT-x, KVM disabled in firmware, or slow boots. #98 must run on such a
  machine before the v1 release.

## Gaps found

- **README has no install steps.** It describes what `sudo boxpool up` does,
  but not how to get a binary (there are no releases yet, so you build from
  source) or that you need `sudo` for `up`. A new user has to piece this
  together from the Commands and Development sections.
- **After `sudo boxpool up`, a regular user cannot use the daemon.** The
  socket is `root:root` mode 0660, so `boxpool sh` without `sudo` fails with
  `permission denied`. Its hint, `start it with boxpool daemon`, points the
  wrong way. The README does not say to use `sudo boxpool sh`. This was
  measured with `sudo`, as above.
- **The issue's command does not exist.** `boxpool sh --timing -- true`:
  `sh` takes no command and needs a terminal. This gate times `sh` under
  `script(1)` instead, with no new feature.
