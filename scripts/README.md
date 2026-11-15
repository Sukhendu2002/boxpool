# Repository tools

Start with `make help` and `make check` at the repository root. These tools use
the shell or Python standard library; the npm launcher has no dependencies to
install. Release artifacts belong in ignored `bin/` or `dist/`, not in source.

## Build, documentation and distribution

| Tool | Consumer / purpose |
| --- | --- |
| [dev-sign.sh](dev-sign.sh) + [vz.entitlements](vz.entitlements) | Ad-hoc sign a Mac development binary or integration test with the virtualization entitlement. Unit tests need no signing. |
| [generate-command-reference.py](generate-command-reference.py) | `make docs` / `make docs-check`; reads CLI help without starting a daemon. |
| [install.sh](install.sh) | Published verbatim at `https://boxpool.run/install` by the Pages workflow; covered by offline installer tests. |
| [prepare-distribution.py](prepare-distribution.py) | Release workflow stages a pinned npm package and Homebrew formula from verified binary/checksum assets. |
| [tests/](tests/) | Unit tests for the installer, distribution generator, command reference and benchmark harnesses. Run `make test-scripts`. |

The landing/docs site has a separate [build and test guide](../site/README.md);
use `npm ci --prefix site`, `make site` and `make test-site`.

Image-building tools stay with their inputs in [image/](../image/README.md).
Security probes stay in [test/security/](../test/security/README.md).

## Explicit hardware checks and benchmarks

These remain supported reproduction tools. They are not part of `make check`.
Read the linked prerequisites before running them: some create sandboxes,
install/start a daemon, crash it deliberately, or prepare a host reboot.

| Tool | Purpose and prerequisites |
| --- | --- |
| [bench-boot.sh](bench-boot.sh) | Upstream Firecracker cold-boot spike; Linux/KVM, Bash 5+ and filesystem tools. Used by the manual Boot benchmark workflow. [Method and recorded results](../docs/benchmarks/m1-boot.md). |
| [bench-gate.sh](bench-gate.sh) | Linux M1 install/boot timing on a dedicated KVM host. Its `selftest` checks the timing parser with stubs. [Reproduction](../docs/benchmarks/m1-gate.md). |
| [check-cli.py](check-cli.py) | Same real shell/exec/file/lifecycle checks on Mac and Linux, against an explicitly selected installed daemon. [M2 gate](../docs/benchmarks/m2-gate.md). |
| [bench-m3.py](bench-m3.py) | Resume, daemon crash and reboot persistence campaign. A reboot check requires a prepared state file and an actual changed host boot ID. [M3 gate](../docs/benchmarks/m3-gate.md). |

Keep benchmark scripts with their recorded measurements so contributors can
reproduce results. A script does not become unused merely because it is not a
per-PR CI step. Hardware measurements and regular unit-test results establish
different properties.
