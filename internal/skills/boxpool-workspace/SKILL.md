---
name: boxpool-workspace
description: Work on a project in a Boxpool sandbox while keeping your agent, model login and personal skills on the host. Use when the user selects Boxpool for project file operations, commands, builds or tests.
---

# Boxpool workspace

The harness runs on the host; the project lives in a Linux guest, even on a Mac
node. Keep the harness's model authentication, personal skills and settings on
the host. Run project reads, writes, edits, searches, Git, dependency installs,
builds, tests and long-running processes through `boxpool exec` in the selected
sandbox. Local harness tools still target the host: this skill guides their use;
it does not intercept or enforce them.

## Bind the session

1. Use the user's selected sandbox and guest project directory. If either is
   missing, inspect `boxpool ls --json` and ask only for the unresolved choice.
   Do not provision, choose another workspace or transfer a repository merely
   because this skill was loaded.
2. Find the selected name in `boxpool ls --json`; verify `state` is `running`.
   Record its name, full `id` and an **absolute guest project path** in the
   session. The current CLI addresses sandboxes by **name**, not ID. Recheck
   that the name still has the recorded ID and running state before a mutation
   batch and after a reconnect. Stop if its identity changes. This cooperative
   check is not atomic with execution. The node's label is a placement selector,
   not a sandbox name.
3. Verify the path and remote tools before project work. In the recipes below,
   replace `SANDBOX_NAME` and `/work/project` with that binding on **every call**.
   Variables and `cd` do not persist across separate host tool calls.

```sh
boxpool exec SANDBOX_NAME -- sh -c 'cd "$1" || exit; pwd; uname -s; for tool in node git grep; do command -v "$tool" || :; done' sh /work/project
```

Read project instructions (`AGENTS.md`, `CLAUDE.md`, contributing guides) from
the guest, including applicable parent directories, before editing. Paths in
project instructions refer to the guest; paths in personal harness setup may
refer to the host. Resolve that distinction before following another skill.

Run Boxpool on the host, not inside the guest. On a Linux host using its local
root-owned daemon, use the installed binary with `sudo -n /usr/local/bin/boxpool`;
on a Mac, or a client configured with `boxpool login`, use `boxpool` as the user.
Do not switch accounts, change `BOXPOOL_HOME` or run `boxpool login/logout`
during a bound session without the user's direction.

## Remote project operations

Flags go **before** the sandbox name. Scripts use non-interactive `exec`, not
`sh` or `exec -t`. Add `-i` only when sending stdin. Boxpool returns the remote
exit status; 125 means Boxpool failed or the guest command itself returned 125.
Preserve stdout/stderr and report failures accurately.

Pass paths as arguments, rather than interpolating them into shell source. This
wrapper changes directory inside the guest and preserves individual arguments:

```sh
boxpool exec SANDBOX_NAME -- sh -c 'cd "$1" && shift && exec "$@"' sh /work/project pwd
boxpool exec SANDBOX_NAME -- sh -c 'cd "$1" && shift && exec "$@"' sh /work/project ls -la
boxpool exec SANDBOX_NAME -- sh -c 'cd "$1" && shift && exec "$@"' sh /work/project sed -n '1,160p' package.json
```

Prefer installed project tools. `node` is included in Boxpool's standard image;
Git, ripgrep, compilers and package-specific tools may need installation in the
guest within the user's task scope. Never run a missing guest tool on the host
as a substitute. For text search, use remote `rg` if available, otherwise `grep`
or `find`, and limit output and generated/dependency directories:

```sh
boxpool exec SANDBOX_NAME -- sh -c 'cd "$1" && shift && exec "$@"' sh /work/project grep -R -n -F --exclude-dir=.git --exclude-dir=node_modules -- 'search text' .
```

`grep`/`rg` exit 1 means no matches. For larger output, narrow the query or read
in chunks; do not assume a host tool's truncated output is the complete file.

Use quoted heredocs for remote scripts so the host shell does not expand their
contents. This creates a **new** file and refuses to overwrite an existing one:

```sh
boxpool exec -i SANDBOX_NAME -- sh -c 'cd "$1" && exec node -' sh /work/project <<'JS'
const fs = require('node:fs');
fs.writeFileSync('notes.txt', 'workspace notes\n', { flag: 'wx' });
JS
```

For an edit, read the current file remotely and check that the intended text
matches exactly once before writing. Adapt the file and strings to the task:

```sh
boxpool exec -i SANDBOX_NAME -- sh -c 'cd "$1" && exec node -' sh /work/project <<'JS'
const fs = require('node:fs');
const file = 'notes.txt';
const before = 'workspace notes';
const after = 'updated workspace notes';
const text = fs.readFileSync(file, 'utf8');
if (!before || text.split(before).length !== 2) {
  throw new Error('expected exactly one match; inspect the file again');
}
fs.writeFileSync(file, text.replace(before, () => after));
JS
```

Run the project's documented install/build/test commands in the same binding;
the following are examples for an npm project, not universal requirements:

```sh
boxpool exec SANDBOX_NAME -- sh -c 'cd "$1" && shift && exec "$@"' sh /work/project npm ci
boxpool exec SANDBOX_NAME -- sh -c 'cd "$1" && shift && exec "$@"' sh /work/project npm test
boxpool exec SANDBOX_NAME -- sh -c 'cd "$1" && shift && exec "$@"' sh /work/project npm run build
boxpool exec SANDBOX_NAME -- sh -c 'cd "$1" && shift && exec "$@"' sh /work/project git diff --check
boxpool exec SANDBOX_NAME -- sh -c 'cd "$1" && shift && exec "$@"' sh /work/project git diff
```

Use remote `git status --short` too: a diff omits untracked files. Do not commit,
push or publish merely because this skill is active. For a background process,
redirect stdin, stdout and stderr inside the guest, record its PID and logs,
and manage it there. Use `boxpool expose` on the head when the task calls for an
app preview; keep access links out of public logs and commits.

## Transfer, failures and handoff

- The guest is the source of truth for this session. Do not silently synchronize
  to a host checkout or use native host read/edit/search tools on project paths.
  The host can still read personal skills and harness configuration needed for
  the task; that does not make its project filesystem the workspace.
- Transfer only when requested. `boxpool cp ./file SANDBOX_NAME:/work/project/`
  uploads into an **existing directory** and may replace a same-named file.
  `boxpool cp SANDBOX_NAME:/work/project/result.txt ./results/` downloads an explicit
  artifact. Inspect the destination first. Do not copy home directories, model
  credentials or personal harness configuration to make project commands work.
  Private repository/registry access is separate: use an explicitly authorized
  project authentication method and never put tokens in commands or transcripts.
- If the sandbox is missing, stopped, unreachable, or the project directory is
  absent, stop project work and report what failed. Do not retry project commands
  locally, pick another sandbox, create a replacement, or reset the project.
  Resume after the intended workspace is available and verify the same binding.
- Do not treat a disconnect during a write or command as proof nothing happened;
  inspect remote state before repeating a mutation.
- At handoff, report the sandbox ID/path, files changed, remote checks and results,
  and any processes still running. Preserve the user's workspace; stop/delete
  resources only when that cleanup was requested.
