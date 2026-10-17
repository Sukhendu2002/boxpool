# Agent and contributor guide

Rules for anyone (human or agent) changing this repo. Product scope lives in the
PRD and technical design; the roadmap lives in the pinned **Roadmap** issue.

## Project

- Go, one binary: `cmd/boxpool`. Packages live under `internal/`.
- Build: `go build -o bin/boxpool ./cmd/boxpool`. Test: `go test ./...`. Vet: `go vet ./...`.
- Code must be `gofmt`-clean. CI runs vet, test and build on Linux and macOS.
- Prefer the standard library. A new dependency needs one line in the PR saying why.
- Every behaviour change ships with a unit test, or with manual test steps in the PR when it cannot be unit-tested (hypervisors, signing, real hardware).

## Workflow

1. Pick the highest-priority open issue in the current milestone that is not blocked (see "Blocked by" on the issue).
2. Branch from `main`: `<type>/<issue-number>-<short-slug>`, e.g. `feat/12-firecracker-driver`.
3. Commit using the format below. Small commits are fine; the PR is squash-merged.
4. Open a PR using the template. One issue per PR. The PR body must contain `Closes #<n>`.
5. Merge only when CI is green and the issue's acceptance criteria are met.

## Commit messages

[Conventional Commits](https://www.conventionalcommits.org/):

```
<type>(<scope>): <summary>

<body: why the change is needed, and anything surprising. Wrap at 72.>

Refs #<issue>
```

- **type**: `feat`, `fix`, `docs`, `test`, `refactor`, `perf`, `build`, `ci`, `chore`, `revert`.
- **scope** (optional, one word): `cli`, `daemon`, `preflight`, `store`, `linux`, `firecracker`, `docker`, `macos`, `guest`, `image`, `pool`, `sched`, `net`, `proxy`, `auth`, `desktop`, `vault`, `api`, `sdk`, `release`, `deps`.
- **summary**: imperative, lower case, no trailing period, 72 characters or fewer. "add firecracker driver", not "Added Firecracker driver."
- Breaking change: `feat(api)!: ...` plus a `BREAKING CHANGE:` footer.
- Agent-written commits end with a `Co-Authored-By:` trailer for the agent.

## Pull requests

- **Title**: same format as a commit summary, e.g. `feat(firecracker): boot a microVM from the base image`. CI rejects other titles. The title becomes the squash commit on `main`.
- **Body**: fill every section of `.github/pull_request_template.md`: Summary, `Closes #n`, Changes, Testing (exact commands and what you saw), Risk, Checklist. Write "None" rather than deleting a section.
- Agent-written PRs end with the agent's generated-by line.

## Issues

Every issue uses one of the forms in `.github/ISSUE_TEMPLATE/` and has these sections, in this order:

- **Summary**: one or two sentences on what to build or fix.
- **Why**: the user need or the risk it retires. Link the PRD requirement (`F4`) when there is one.
- **Scope**: in scope and out of scope.
- **Acceptance criteria**: a checklist. Each item can be verified.
- **Testing**: *Unit* (what to unit-test), *Integration* (automated tests that need a real backend), *Manual* (numbered steps with commands and expected output).
- **Dependencies**: `Blocked by #n` lines, also set as GitHub issue dependencies.

Titles use the commit format: `feat(firecracker): boot a microVM from the base image`.

Labels:

| Group | Labels | Meaning |
| --- | --- | --- |
| Priority | `P0`, `P1`, `P2` | Must ship in v1 / should ship in v1 / later |
| Type | `type: feature`, `type: bug`, `type: chore`, `type: spike`, `type: docs`, `type: gate`, `type: epic` | What kind of work. A gate is a milestone's exit check; an epic groups a milestone's issues as sub-issues |
| Area | `area: *` | Which part of the system |

Milestones `M1`–`M7` follow the PRD; each ends with a gate issue that must pass before the next milestone starts. Work order: open the current milestone's epic, then take the first sub-issue whose blockers are closed, P0 before P1. New issues go under their milestone's epic as a sub-issue.
