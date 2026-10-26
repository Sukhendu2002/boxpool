# Preinstalled coding agents (issue #23)

The default image adds the versions in `image/versions.txt`: Node.js
24.22.1, Claude Code 2.1.310, Codex 0.164.0 and Gemini CLI 0.65.0. None
is authenticated or started at boot.

## Size and boot comparison

Measured on an Apple M2 MacBook Air, 16 GiB RAM, macOS 27, using the vz
driver and images built from `cffabb0410f6` before and after this change.
Both use the same kernel, one vCPU, 1024 MiB RAM and a 2 GiB sandbox disk.
The base filesystem grows from 512 MiB to 2 GiB to fit the agents.

| arm64 artifact | Before | With agents |
| --- | ---: | ---: |
| Compressed ext4, bytes | 26,317,403 | 328,987,703 |
| Base filesystem capacity | 512 MiB | 2 GiB |
| Used guest filesystem (rounded) | 94 MiB | 1.1 GiB |
| Median Create | 10.12 ms | 18.02 ms |
| Median Start | 644.73 ms | 979.02 ms |

Start runs from the driver call until the guest agent responds and disk
growth completes. Create is reported separately. Each sample creates a
fresh VM and writable APFS clone; the first VM is discarded and the next
ten are measured. The table uses the upper middle sample. Host caches
are warm: these are fresh-sandbox boots, not host-cache-cold boots, and
exclude downloads, decompression and interactive CLI startup.

The observed Start regression is **51.8%**, exceeding the issue's 10%
threshold. This repeat on October 26 used the committed test harness,
with about 69 GiB host disk space free and no concurrent image build.
Earlier measurements with only 3–6 GiB free also exceeded 10% (one pair:
584.46 ms before, 751.69 ms after). A control expanding the old image
to 2 GiB still booted faster, so capacity alone did not explain the
regression. Rebooting an existing disk reduced mount time considerably;
that is a different workload from the fresh-sandbox measurement.

The image CI tests the OCI userland on native amd64 and arm64 runners.
It also boots the actual amd64 ext4 image with Firecracker and executes
all three version commands through the guest agent. The local vz test
does the corresponding arm64 VM check. These are functional checks;
the Mac timings do not establish Firecracker performance.

## Proposed slim variant

Keep agents in the default image, as required by the v1 decision. For
workloads that do not need them, propose an explicit `slim` image flavor:

- Factor the shared Debian packages, init and boxpool agent into a base
  stage; derive the default stage by adding Node and the three CLIs.
- Export separate `-slim` OCI tags and VM artifacts with the old 512 MiB
  base filesystem. Give them their own checksums and release pins.
- Add explicit daemon image-flavor selection before fetching artifacts,
  keeping the existing default. Persist the flavor with each sandbox so
  later restarts use the same base. Never switch an existing disk's flavor.
- Run shared smoke checks for both flavors and CLI version checks for the
  default. Benchmark both on the same host before publishing the option.

This PR proposes that variant under issue #23's regression exception;
it does not expose a slim flavor or change artifact release pins. The
default image needs at least a 2 GiB sandbox disk; release integration
tests and examples requesting 1 GiB will need to move to 2 GiB when the
new image is pinned. The normal 10 GiB default remains sufficient.

## Reproduce on Apple silicon

Build the old and new images in separate checkouts with the same kernel
configuration (`OUTPUT_DIR=/absolute/output image/build.sh arm64`). Then
build and sign the opt-in test from this branch:

```sh
go test -c -tags integration -o /tmp/boxpool-image-bench.test ./internal/backend/vz
scripts/dev-sign.sh /tmp/boxpool-image-bench.test
BENCH_HOME=/tmp/boxpool-bench-before \
BENCH_KERNEL=/absolute/before/current/vmlinux-arm64 \
BENCH_ROOTFS=/absolute/before/current/rootfs-arm64.ext4.zst \
  /tmp/boxpool-image-bench.test -test.run '^TestImageBoot$' -test.v -test.timeout 4m
BENCH_HOME=/tmp/boxpool-bench-after BENCH_AGENTS=1 \
BENCH_KERNEL=/absolute/after/current/vmlinux-arm64 \
BENCH_ROOTFS=/absolute/after/current/rootfs-arm64.ext4.zst \
  /tmp/boxpool-image-bench.test -test.run '^TestImageBoot$' -test.v -test.timeout 4m
```

Use distinct scratch homes and leave at least 2 GiB available beyond
the decompressed artifacts. Do not run image builds during measurement.
`BENCH_AGENTS=1` additionally requires all three version commands to
succeed in the first guest, outside the timed samples. Without the three
path variables, the test skips, including in ordinary integration runs.
