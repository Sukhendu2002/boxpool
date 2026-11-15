# Local agent / remote workspace verification

Date: 2026-11-15. Scope: issue #147's skill and explicit installer. This is
workflow evidence, not a guarantee that a model always chooses remote tools.

## Setup and method

- Host harness: Pi 1.5.2 on an Apple silicon Mac, macOS 27.2.1.
- Pool: installed v0.1.4 Mac head and Linux amd64 worker labelled `linux-beta`.
- Guest: dedicated Firecracker sandbox `skill-e2e-147`, ID `c310a6c262cb`,
  1 CPU / 1 GiB RAM / 3 GiB disk, standard pinned image. Node v24.22.1;
  Git 2.39.5 installed in this disposable guest for the test.
- CLI: source-built `bin/boxpool` from this change. Only the skill installer
  is new; workspace commands use the existing v0.1.4 daemon/guest protocol.
- Project: `/root/work/skill test's $workspace-v2`, deliberately containing
  spaces, a quote and a dollar sign. The Mac session directory was empty and
  had no project checkout.

Built the CLI and ran `./bin/boxpool skills install`. Pi discovered the installed
`~/.agents/skills/boxpool-workspace/SKILL.md` automatically: RPC `get_commands`
returned `skill:boxpool-workspace`, source `skill`, scope `user`, automatic discovery.
No model authentication or harness settings were copied or changed.

Pi was started in the empty host directory with:

```sh
pi --mode rpc --no-session --offline --no-extensions --no-mcp \
  --no-prompt-templates --no-themes --no-context-files --no-approve
```

The verification driver submitted the skill's command recipes through Pi's RPC
`bash` command, checked exit codes/output and recorded responses. It sent no
model prompts. This tests Pi discovery, host-shell execution and real pool
transport; it does **not** test autonomous model adherence, another harness,
normal user extensions, automatic interception or access restrictions.

## Observed results

| Operation | Result |
| --- | --- |
| Resolve name, ID and running state with `ls --json` | PASS; guest owned by the Linux worker |
| Remote `pwd`, `uname -s`, tool checks | PASS; exact quoted guest path, Linux, Node/Git versions |
| Read guest `AGENTS.md`, list and search project files | PASS through `exec` |
| Generate package lock; `npm ci --ignore-scripts --no-audit --no-fund` | PASS in guest; fixture has no external dependencies |
| Known arithmetic bug, `npm test` | Exit 1; one assertion failed (`-1 !== 5`) |
| Create file with `flag: 'wx'` | PASS; repeated creation failed without replacing content |
| Edit exactly one matching string | PASS; stale repeat failed without changing the file |
| `npm test` after edit | Exit 0; one passing test, zero failures |
| `npm run build` and read `dist/math.js` | PASS; generated artifact contained the corrected expression |
| `git diff --check`, `git diff`, `git status --short` | PASS; one-line fix and untracked notes file visible |
| Missing guest project directory | Exit 2; command after `cd` did not execute |
| Read while the dedicated sandbox was stopped | Exit 125; no local fallback command was submitted |
| Restart and recheck identity/files | PASS; same ID, path and edited disk contents |
| Inspect host session directory | Empty; no project artifacts created locally |
| Cleanup | Test sandbox stopped/removed; both nodes ready; `ls --json` returned `[]` |

The build fixture copies its JavaScript module to `dist`; this proves artifact
placement and command routing, not production build performance. The missing
directory check also verified that `/root/fallback-sentinel` was absent after
restart. A stopped guest was tested; no live worker/network outage was injected
in this run.

## Command details

The driver used separate argv entries for the sandbox name, project directory
and command. The shell equivalent of the working-directory wrapper is:

```sh
./bin/boxpool exec skill-e2e-147 -- sh -c \
  'cd "$1" && shift && exec "$@"' sh "/root/work/skill test's \$workspace-v2" npm test
```

The fixture initially contained:

```js
exports.add = (a, b) => a - b;
```

Its Node test asserted `add(2, 3) === 5`. The remote edit sent this script as
stdin to `exec -i ... node -`, using a quoted heredoc:

```js
const fs = require('node:fs');
const file = 'math.js';
const before = 'a - b';
const after = 'a + b';
const text = fs.readFileSync(file, 'utf8');
if (!before || text.split(before).length !== 2) {
  throw new Error('expected exactly one match');
}
fs.writeFileSync(file, text.replace(before, () => after));
```

The resulting remote diff was:

```diff
-exports.add = (a, b) => a - b;
+exports.add = (a, b) => a + b;
```

`git status --short` also returned `?? notes.txt`, confirming why a diff alone
does not show every new file. The workspace was stopped and started with
`./bin/boxpool stop skill-e2e-147` and `./bin/boxpool start skill-e2e-147`, then
deleted after verification.

## Installer and repository checks

- Installer unit/CLI tests cover custom paths (including spaces), identical
  installs, preserving edited instructions, explicit replacement, unrelated
  files, symlinks/hard links, invalid paths/arguments and help without writes.
- Skill-creator's `quick_validate.py` passed using temporary `uv run --with
  PyYAML`; PyYAML is not a project/runtime dependency.
- `go vet ./...`, `go test ./...`, `go build ./...` passed on the Mac.
- `python3 -m unittest discover -s scripts -p 'test_*.py'`: 18 passed.
- `node --test npm/test/*.test.js`: 9 passed.
- `python3 scripts/generate-command-reference.py --binary bin/boxpool --check`:
  current. Go formatting and `git diff --check`: clean.

The first full Go run failed in the unchanged
`TestRunWaitsForRollbackAfterShutdownTimeout`: it consumed the warning about
port 7443 being busy instead of the shutdown warning it expected. After removing
the disposable sandbox, temporarily unloading the installed Mac launch agent
freed that port; the full suite passed. The same installed v0.1.4 launch agent
was restored and both pool nodes were verified ready. No daemon/backend code
was changed for this work.

## Next behavioral check

Run the [first-session procedure](../local-agents.md#verify-a-first-session)
with a live model in the user's normal Pi setup, then in other harnesses as
needed. Watch that native file tools are not used for project operations. A
shared skill is guidance; enforced tool routing would require a separate
integration. Sandbox identity verification is also cooperative: v0.1.4 targets
names, so checking an observed ID does not atomically pin the next command.
