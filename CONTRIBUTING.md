# Contributing to Boxpool

Start with an [open issue](https://github.com/Sukhendu2002/boxpool/issues) or
report a reproducible bug using the issue forms. The
[roadmap](https://github.com/Sukhendu2002/boxpool/issues/64) explains the planned
work; use each issue's current state, milestone and blockers when choosing a
task. For a larger change, agree on its scope in an issue before implementation.

## First local check

You can work on the CLI, daemon, documentation and unit tests without starting
Boxpool or having a hypervisor. Use a Linux host or Apple silicon Mac with:

- Git and Go 1.25+ (see [go.mod](go.mod) for the toolchain baseline).
- Node.js 20+ for launcher tests; the npm package has no dependencies.
- Python 3.8+ for tooling tests; they use the standard library.
- Make for the commands below. On macOS, also install Xcode Command Line Tools
  (`xcode-select --install`); the Virtualization framework binding uses cgo.

```sh
git clone https://github.com/Sukhendu2002/boxpool.git
cd boxpool
make help
make check
```

`make check` checks Go formatting, runs vet and the Go/Python/Node unit suites,
builds `bin/boxpool`, and checks generated command documentation. It does not
install the binary, replace a running daemon, create VMs or require credentials.
The first Go run may download modules and the required toolchain. CI runs the
same entry point on Linux and macOS, with additional shell linting, fuzzing and
platform integration jobs.

Use `make test`, `make test-scripts` or `make test-npm` while iterating. For a
single Go package, use the normal Go commands, for example
`go test ./internal/client`. Run `make fmt` after editing Go files. If you
change CLI help, run `make docs` and include the generated diff in your PR.

Make is a convenience, not a runtime dependency. The equivalent direct commands
are in [Makefile](Makefile), and the required Go checks remain:

```sh
go vet ./...
go test ./...
go build -o bin/boxpool ./cmd/boxpool
```

## Find the code

| Path | Purpose |
| --- | --- |
| [cmd/boxpool/](cmd/boxpool/) | Host CLI, service setup and daemon entry point |
| [cmd/boxpool-agent/](cmd/boxpool-agent/) | Guest agent entry point |
| [internal/client/](internal/client/) | Client used by the CLI to reach the daemon |
| [internal/daemon/](internal/daemon/) | Sandbox lifecycle, pool coordination, auth and HTTP routes |
| [internal/backend/](internal/backend/) | Shared guest setup and Firecracker, VZ and Docker drivers |
| [internal/agent/](internal/agent/) | Guest exec/file protocol and browser desktop assets |
| [internal/store/](internal/store/) | Persistent state and migrations |
| [internal/transport/](internal/transport/) | Node connectivity over direct/tailnet TCP |
| [internal/auth/](internal/auth/), [internal/resources/](internal/resources/), [internal/preflight/](internal/preflight/) | Trust, resource admission and host checks |
| [internal/artifacts/](internal/artifacts/), [internal/diskspace/](internal/diskspace/), [internal/power/](internal/power/) | Pinned image downloads, disk capacity and host sleep/power reporting |
| [internal/skills/](internal/skills/) | Bundled local-agent workspace skill and installer |
| [image/](image/) | Guest images, pinned tools, kernels and image tests |
| [npm/](npm/) and [packaging/](packaging/) | npm launcher and Homebrew formula template |
| [scripts/](scripts/README.md) | Maintained tools, benchmarks and tooling tests |
| [test/security/](test/security/README.md) | Explicit host/backend security probes |
| [site/](site/README.md) | Landing page, Markdown docs site and Go module discovery |
| [.github/workflows/](.github/workflows/) | Unit, integration, image, release and site pipelines |

Go unit tests live beside the packages they exercise. Python tooling tests live
in `scripts/tests/`; npm launcher tests live in `npm/test/`. The
[documentation index](docs/README.md) separates user guides, implementation
references and dated hardware evidence.

## Work on the website

The website publishes the same Markdown guides that live in `docs/`.
For landing page or documentation-site changes, use Node.js 20.19+ and run
`npm ci --prefix site`, `make site` and `make test-site`. See the [site guide](site/README.md) for local
preview, navigation and deployment details. Website dependencies are build-only
and separate from the dependency-free npm CLI launcher.

## Test a real backend when needed

Changes to virtualization, networking, image startup, signing or service setup
need real-backend checks in addition to unit tests. Follow the relevant section
of [development.md](docs/development.md):

- [Apple silicon Mac setup and signed VZ tests](docs/development.md#macos-on-apple-silicon).
- [Linux build/setup](docs/development.md#build-and-verify-on-linux) and
  [KVM integration tests](docs/development.md#linux-testing-for-each-pr).
- [Guest image builds and smoke tests](image/README.md).
- [Security probes and their prerequisites](test/security/README.md).

Hardware tests can create VMs, download images or exercise host networking;
run them explicitly on a suitable development machine. Keep their state and
resource requirements separate from an existing work pool. GitHub's KVM and
Docker jobs provide automated coverage, but Mac VZ tests and performance/reboot
measurements still need suitable hardware. Record the commands, host/backend
and observed results in the PR; explain any checks you could not run.

## Submit a change

[AGENTS.md](AGENTS.md) is the shared source of contribution rules for humans
and agents: issue structure, branch names, commit format and PR requirements.
In particular:

1. Use one issue per PR. Branch from current `main` as
   `<type>/<issue-number>-<short-slug>`.
2. Prefer the standard library; explain a new dependency in the PR.
3. Add unit coverage for behavior changes, or exact manual steps when a unit
   test cannot establish the result.
4. Run `make check` and any backend checks your change needs.
5. Use a Conventional Commit title and fill every section of the
   [PR template](.github/pull_request_template.md), including `Closes #n`,
   commands/results, risk and checklist. Agent-written work also follows
   the attribution rules in AGENTS.md.

The project squash-merges after CI is green and acceptance criteria are met.
Do not include generated binaries, downloaded VM images, local caches, model
credentials or private test access links in a contribution.
