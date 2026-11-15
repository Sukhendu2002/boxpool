# Documentation index

## Use Boxpool

- [Two-machine quickstart](../README.md#quickstart-a-two-machine-pool): install,
  start a head, join a worker and run a sandbox.
- [User guide](guide.md): files, resources, persistence, placement and clients.
- [Command reference](commands.md): generated from this checkout's CLI help.
- [Local agents](local-agents.md): keep your harness on the laptop and project
  work in a sandbox.
- [App previews](expose.md) and [desktop takeover](desktop.md).
- [Troubleshooting](troubleshooting.md): host checks, startup and connectivity.
- [Security and trust model](security.md): isolation boundaries and limitations.

## Contribute and understand the implementation

- [Contributing](../CONTRIBUTING.md): prerequisites, repository map, ordinary
  checks and the issue/PR workflow. Start here for a first contribution.
- [Development reference](development.md): platform setup, backend tests,
  signing, releases and detailed manual checks.
- [Guest image reference](../image/README.md): image build, pins and smoke tests.
- [Guest agent protocol](agent-protocol.md): private host-to-guest interface.
- [Distribution maintenance](distribution.md): installer, npm, Homebrew and site.
- [Scripts catalog](../scripts/README.md) and [security probes](../test/security/README.md).
- [Shared contribution rules](../AGENTS.md).

## Validation and measured results

- [Independent onboarding procedure](onboarding-check.md).
- [M1 cold-boot spike](benchmarks/m1-boot.md) and [Linux gate](benchmarks/m1-gate.md).
- [M2 Mac/CLI gate](benchmarks/m2-gate.md) and [bundled coding agents](benchmarks/m2-agents.md).
- [M3 persistence gate](benchmarks/m3-gate.md), [Mac pause](benchmarks/m3-macos-pause.md)
  and [warm boot](benchmarks/m3-warm-boot.md).
- [M4 join measurements](benchmarks/m4-join.md).
- [Image rollout verification](benchmarks/image-rollout.md).
- [Local-agent skill verification](benchmarks/local-agent-skill.md).

Benchmark documents are dated evidence, not a statement that today's release
has passed every gate. Read their recorded versions, status and remaining
checks. The [roadmap issue](https://github.com/Sukhendu2002/boxpool/issues/64)
and individual issue states track planned work.
