# M2 gate: macOS and the shared CLI

Issue #25. Measured on 2026-10-28 using the published
[`v0.1.0-alpha.1` release](https://github.com/Sukhendu2002/boxpool/releases/tag/v0.1.0-alpha.1),
commit `6c61b54bd6e9be66d5c05019622dbb90e9e9ee26`.

**Status: PASS.** On the Apple M2, ten fresh sandboxes reached the shell
prompt in **689 ms median, 781 ms p90**, below the 2 s target. The same
unchanged CLI script passed on this Mac and the bare-metal Linux reference
machine. Raw results are committed alongside this document.

## Acceptance criteria

| Criterion | Evidence | Result |
| --- | --- | --- |
| Cold boot under 2 s median on an M1 or newer Mac | M2, 10 new VMs, 689 ms median | PASS |
| Same `sh`/`exec`/`cp`/`ls`/`rm` script on Linux and macOS | `scripts/check-cli.py`, both runs completed with no owned sandboxes left | PASS |
| Results committed | This document, [Mac samples](m2-gate/macos.json), [Linux samples](m2-gate/linux.json) | PASS |

## Machines and release

The Mac is an Apple M2 MacBook Air, 8 CPU cores, 16 GiB RAM, on AC power;
macOS 27.0.1, Darwin 27.0.0, APFS on its internal SSD (245.1 GB). About
44 GB was free. The release's ad-hoc signature, hardened runtime and
`com.apple.security.virtualization = true` entitlement were verified before
installation. No Developer ID membership or Apple secrets were used.

The Linux reference laptop is a bare-metal Intel Core i5-1235U, 12 threads,
19.2 GiB RAM, NVMe, Ubuntu 24.04.3, kernel `7.0.0-38-generic`.
`systemd-detect-virt` reported `none`, `/dev/kvm` was accessible to root,
and `doctor` selected Firecracker. About 33 GiB was free before installation.
These are reference-hardware measurements, not nested hosted-runner numbers.

Both CLI and daemon reported `v0.1.0-alpha.1`. SHA-256 of the published
binaries used here:

```text
72aed8f8fbc1f67ee4aa12f3adb69dd8d2a44304d73c829e83b0f39abbd88dbe  boxpool-darwin-arm64
cbabf52878419efac68b8ab2714ec120dfef607e234d3ee2397ee5d5af5a4899  boxpool-linux-amd64
```

The release pins guest image `v0.0.1-alpha.1`, with its existing 512 MiB
base filesystem, and Firecracker `v1.17.0` on Linux. This gate tests that
published distribution. The larger agent-equipped image from #23 is not
yet pinned by this release; its size/boot measurements and slim proposal
are in [m2-agents.md](m2-agents.md). These timings do not establish a boot
time for a future image pin.

## Method

[`scripts/check-cli.py`](../../scripts/check-cli.py) uses Python 3.8+ and
its standard library on both hosts. The script's SHA-256 was identical on
both machines:

```text
82004392a40a791636738b2eb5aaef90604387f659b536eba277a0d248186f30
```

On the Mac, `boxpool up` installed the downloaded release as the current
user's launch agent, using a fresh `BOXPOOL_HOME` under `/tmp`. It reported
healthy after 10.9 s, including initial image downloads. On Linux,
`sudo boxpool up` installed that platform's downloaded release as the root
systemd service in `/var/lib/boxpool`, reporting healthy after 9.4 s.
Neither machine had an existing boxpool service or installation to replace.

The issue's example `boxpool sh --timing -- true` is not supported: `sh`
takes a sandbox name and requires a terminal. The harness opens a PTY,
starts `boxpool sh --timing --name <unique-name>`, and waits until the real
root prompt is visible before typing `exit`. It rejects a banner as the
first output after the timing, missing prompts/timings, nonzero shell
exits, hangs, failed removals, wrong backends and non-default resource sizes.

Every timed invocation creates a new VM and writable disk with the defaults:
2 vCPU, 2048 MiB RAM and 10 GiB disk. The daemon and downloaded artifacts
are already initialized. Host filesystem/page caches are warm from setup
and validation; caches were not dropped. This is a cold **sandbox create
and boot**, not a cold host reboot, cold-disk read or download benchmark.
All ten recorded samples are included, including the first; none was
discarded as a warmup or outlier.

`--timing` starts at entry to the CLI command and ends on the login shell's
first output: daemon requests, APFS clone or Linux disk copy/resize, VM
boot, guest-agent readiness and starting `bash -l` are included. The
harness confirms that first output is the prompt. It then removes the VM
before the next sample, so accumulated live VMs cannot change admission.
Median is the mean of the middle two sorted values; p90 is nearest rank,
the ninth sorted sample of ten.

Example Mac transcript, with terminal escape sequences and CRs removed:

```text
sandbox m2-4c3d64c7b918-1 (vz)
time to prompt: 849ms
root@192:~# exit
logout
sandbox m2-4c3d64c7b918-1 is still running; reattach with: boxpool sh m2-4c3d64c7b918-1
```

## Raw cold boots

Time to prompt in milliseconds; these match the committed JSON results.

| Run | Mac M2, vz | Linux reference, Firecracker |
| --- | ---: | ---: |
| 1 | 849 | 416 |
| 2 | 679 | 403 |
| 3 | 680 | 420 |
| 4 | 712 | 444 |
| 5 | 670 | 396 |
| 6 | 660 | 399 |
| 7 | 698 | 445 |
| 8 | 640 | 447 |
| 9 | 715 | 448 |
| 10 | 781 | 430 |
| Median | **689** | **425** |
| p90 | **781** | **447** |

The Linux column validates the same script and release on the second
platform. The M2 gate's performance target applies to the Mac.

## Shared CLI session

After the timed boots, the same script performs these assertions on each
host, with no platform branches in the session logic:

1. Create a fresh sandbox with `sh`, exit its shell, and attach again with
   `sh`; the sandbox ID must stay the same.
2. `exec` a guest command which writes separate stdout/stderr and exits 7;
   check both byte streams and the exit status. `exec -i ... cat` must echo
   stdin and finish at EOF. `uname -m` must match the host architecture.
3. `cp` a file containing a NUL and every byte value into the guest's
   existing `/tmp/` directory, then copy it back to a new host filename.
   Filenames contain spaces; the returned bytes must match exactly.
4. Check the timed sandboxes' backend/state and default resources through
   `ls --json`, and the session sandbox in JSON and the human-readable
   `ls` table.
5. Refuse plain `rm` while the sandbox runs; `stop` must report a stopped
   sandbox, `exec` against it must fail with 125, and plain `rm` must then
   remove it.
6. Require the original sandbox list to be restored. On failure, remove
   only this invocation's randomly named sandboxes; preserve unrelated
   sandboxes. Keep per-shell transcripts and command stdout/stderr/status.

Both hosts reported:

```text
PASS: sh/reattach, exec streams/exit/stdin, cp binary round trip, ls, stop, rm
remaining_owned_sandboxes: []
```

## Reproduce

Use the published release and a spare host/account. On macOS, confirm there
is no existing `run.boxpool.daemon` launch agent before these steps:
`BOXPOOL_HOME` isolates state but `up` uses the same launchd label and plist.
Follow the [release verification/Gatekeeper instructions](../development.md#releases)
for a trusted download; no global Gatekeeper setting needs changing.

From the repository root, on an Apple silicon Mac:

```sh
gate_dir=$(mktemp -d /tmp/boxpool-m2-gate.XXXXXX)
gh release download v0.1.0-alpha.1 --repo Sukhendu2002/boxpool \
  --pattern 'boxpool-*' --pattern checksums.txt --dir "$gate_dir/release"
(cd "$gate_dir/release" && shasum -a 256 -c checksums.txt)
chmod +x "$gate_dir/release/boxpool-darwin-arm64"
export BOXPOOL_HOME="$gate_dir/state"
"$gate_dir/release/boxpool-darwin-arm64" up
python3 scripts/check-cli.py --backend vz --runs 10 \
  --log-dir "$gate_dir/logs" "$BOXPOOL_HOME/bin/boxpool"
"$BOXPOOL_HOME/bin/boxpool" down
```

On the Linux reference host, verify the matching release binary/checksum,
then, with no existing boxpool installation:

```sh
chmod +x ./boxpool-linux-amd64
sudo ./boxpool-linux-amd64 up
sudo python3 scripts/check-cli.py --backend firecracker --runs 10 \
  --log-dir /tmp/boxpool-m2-linux-logs /usr/local/bin/boxpool
sudo boxpool down
```

The script exits nonzero if any assertion fails or the median is not below
2000 ms. Read `results.json`, `commands.log` and individual shell transcripts
in the selected log directory. The measured Mac run used
`/tmp/boxpool-m2-gate.szxsTZ/recorded-logs`; Linux used
`/tmp/boxpool-m2-gate.YZM23T/recorded-logs`.

## Integration checks and cleanup

The harness regression tests run in the existing Linux/macOS CI matrix:

```sh
python3 -m unittest discover -s scripts -p 'test_*.py'
```

They cover duration/terminal parsing, preservation of unrelated sandboxes,
missing timings/prompts, output preceding the prompt, nonzero shell exits,
hangs and failed cleanup. The same tests also passed locally on both hosts.

Real macOS VM integration runs locally, not on hosted CI: hosted Macs
report `Virtualization is not available on this hardware`
([recorded hosted failure](https://github.com/Sukhendu2002/boxpool/actions/runs/40362275603/job/121710660434)).
The local signed driver suite and launch-agent integration commands are:

```sh
go test -c -tags integration -o /tmp/boxpool-m2-vz.test ./internal/backend/vz
codesign --force --sign - --options runtime --timestamp=none \
  --entitlements scripts/vz.entitlements /tmp/boxpool-m2-vz.test
/tmp/boxpool-m2-vz.test -test.v -test.timeout 5m
go test -tags integration -run '^TestLaunchAgent$' -v ./cmd/boxpool
```

Both commands passed on this Mac, including the VM lifecycle, networking
and launch-agent cleanup checks. Only the opt-in local-image benchmark
`TestImageBoot` skipped because no benchmark image was selected.

After the gate runs, both sandbox lists were empty. `down` removed the Mac
launch agent and plist; no Virtualization VM helpers remained. Its scratch
state and transcripts were retained. On Linux, the service, binary, unit
and `/var/lib/boxpool` created for this test were removed, restoring their
initial absence. No Firecracker processes or `bp-*` TAPs remained, and the
before/after `nft list tables` outputs were identical.

## Decision

The M2 gate passes for the published release on this M2 Mac and the Linux
reference host. Proceed to M3 after this gate PR's CI is green and the
results are merged. This does not certify other Mac hardware/OS versions,
cold host caches, or a future guest-image pin.
