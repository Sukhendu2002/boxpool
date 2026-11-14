# Updated guest image rollout

Issue #135, 2026-11-14. Image release
[`v0.1.2-image.3`](https://github.com/Sukhendu2002/boxpool/releases/tag/v0.1.2-image.3)
was built from `1b5449838321c6b0c0efcf743f4fc05d39023526` after the init
and APT fixes. Failed image releases were not retagged or reused.

## Published artifact verification

The [release run](https://github.com/Sukhendu2002/boxpool/actions/runs/43609079121)
passed all four native base/desktop builds, both desktop Chromium capture
checks, both amd64 KVM boot smokes, and guest-image publication.

- All 15 distinct VM assets' GitHub upload SHA-256 digests match the
  published `SHA256SUMS`; exact byte sizes were read from release metadata.
  The six kernel/rootfs pins in `internal/artifacts/artifacts.go` use these
  verified digests and sizes.
- Both GHCR indexes and their child manifests/configuration blobs were
  fetched anonymously and checked against their SHA-256 digests. Each index
  contains exactly Linux amd64 and arm64; configuration labels match the
  release version and source revision above.
- Base index: `sha256:5fe9553b95799a5af4427279ea45cf9666dcb3e7db0ebbd66aa2088bde454734`.
- Desktop index: `sha256:25256b5eec5c09adea93d145be01b393a7b73359084346deedd14e441bea9aba`.

## Consumer validation

Local macOS arm64 unit tests, vet, build, cache/download race tests,
generated command reference, and workflow lint passed. Unit tests cover
legacy cache isolation, lazy desktop downloads, cancellation/corruption,
and explicit image overrides. Linux CI requires both default images and
desktop startup through the actual Firecracker and Docker drivers.

Real Mac checks passed on macOS 27.2.1, Apple silicon arm64, APFS:

- Default desktop: automatic base/desktop downloads, Chromium
  `155.0.8059.79`, a visible browser window, a nonempty screenshot, and
  read-only keyboard/pointer policy. Passed in 200.48 s including downloads.
- Legacy disk upgrade: an alpha.1 sandbox kept its file and original
  `v0.0.1-alpha.1` agent after reopening with the new driver defaults; a
  fresh sandbox selected `v0.1.2-image.3`. Passed in 3.26 s.
- Base VM: three boots, stop/start persistence, CPU/memory/disk limits,
  clean filesystem check and the unclean-stop fallback. Passed in 10.77 s.
  Guest tools reported Claude Code `2.1.310`, Codex `0.164.0` and Gemini
  `0.65.0`; the agent advertised TCP forwarding and shutdown support.

To reproduce, download the verified legacy fixture described below and run
a signed integration binary with a private artifact cache:

```sh
go test -c -tags integration -o /tmp/boxpool-135-vz.test ./internal/backend/vz
scripts/dev-sign.sh /tmp/boxpool-135-vz.test
BOXPOOL_TEST_HOME=/tmp/boxpool-135-mac \
BOXPOOL_TEST_LEGACY_ROOTFS=/tmp/boxpool-135-legacy/rootfs-arm64.ext4.zst \
  /tmp/boxpool-135-vz.test -test.v -test.count=1 -test.timeout 20m \
  -test.run '^TestIntegration(Desktop|ImageUpgrade)?$'
```

The legacy fixture is the `v0.0.1-alpha.1` arm64 compressed rootfs, verified
against SHA-256 `e5246037ad7241f0ca791c8f4552644c0739ee0ded3d1edac7255becc29ae3d2`.
Tests own their VM state and do not replace an installed daemon.

## After the consumer PR merges

Publish a new consumer binary release from the merged pins and validate the
installer, npm launcher and Homebrew formula against that version. Then
complete #135's consumer-release acceptance criterion. Existing sandbox
disks and snapshots retain their original agent; updating their agent
requires backing up work and creating a fresh sandbox.

The image prerelease's npm job recognized the tarball and generated
provenance, but the registry rejected its publish with `E404`. Public
`boxpool@0.1.1` remains available. Registry authorization must be resolved
before the consumer npm release; see the [distribution runbook](../distribution.md#npm-bootstrap-and-releases).
This npm failure did not prevent VM assets or GHCR publication.
