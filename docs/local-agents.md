# Keep your agent local, use a Boxpool workspace

Run Pi, Codex or Claude Code on your laptop with your existing model login,
personal skills and settings. Keep the project's checkout, dependencies, file
operations, builds and tests in a Boxpool sandbox. The bundled guest agents are
also available when you prefer to run the whole harness in the sandbox.

The `boxpool-workspace` skill teaches this workflow using the existing `exec`
and `cp` commands. It guides the agent; it does **not** intercept native harness
tools, restrict host access or enforce a security boundary. Check tool calls
during the first session. A skill that uses local paths or native file tools may
need to be adapted. MCP and native tool adapters are outside this first version.

## Install the skill on your agent's machine

`boxpool skills install` is new in this checkout; v0.1.4 does not include the
installer. Until a release contains it, follow the [source build instructions](guide.md#installation-options)
and use `./bin/boxpool skills install` from the checkout. The skill's workspace
commands also work with an already installed v0.1.4 CLI and daemon.

```sh
# Pi and Codex: shared user skills directory (~/.agents/skills).
boxpool skills install

# Claude Code: its user skills directory.
boxpool skills install --dir ~/.claude/skills

# Or install under another harness's configured skills directory.
boxpool skills install --dir /absolute/path/to/skills
```

Run this as your normal user, including on Linux: it installs harness
instructions, not a daemon. It writes only `boxpool-workspace/SKILL.md` under the
chosen directory, without inspecting model credentials or modifying harness
settings. It does not create or select a sandbox.

Identical installs are kept. If the file differs, review your edits before
running the same install command with `--force`; that replaces `SKILL.md` with
the bundled version and leaves other files alone. To uninstall, remove that
installed file, then reload the harness. Each harness may have its own installed
copy; update the directories you actually use after upgrading Boxpool.

Restart or reload your harness. In Pi, invoke `/skill:boxpool-workspace`. In
Codex, select `$boxpool-workspace`. In Claude Code, invoke
`/boxpool-workspace`. Other harnesses must support Agent Skills or load the
installed `SKILL.md` as instructions; merely installing a file does not ensure
every harness discovers or follows it.

For a temporary Pi trial without installing globally, build from this checkout
and start Pi with the skill explicitly loaded:

```sh
pi --skill /absolute/path/to/boxpool/internal/skills/boxpool-workspace/SKILL.md
```

## Prepare a workspace

Use an existing running sandbox, or create one deliberately from an interactive
terminal on the head. For example, if a worker has the label `linux-beta`:

```sh
boxpool ls --nodes
boxpool sh --node linux-beta --name agent-work --cpu 2 --mem 2g --disk 6g
```

Inside the guest, prepare a project and its development tools using your usual
Linux setup, then `exit` back to the host. Guests include Node.js, but tools such
as Git, ripgrep and compilers may need installing. For a public repository on a
standard Debian guest, for example:

```sh
apt-get update
apt-get install -y git ripgrep
mkdir -p /root/work
# Replace this with your own public repository URL.
git clone https://github.com/OWNER/REPO.git /root/work/project
exit
```

For a private repository, arrange project authentication separately. The local
model login does not grant Git or registry access to the guest. Do not transfer
your home directory or model authentication to solve that problem.

Alternatively, explicitly copy selected source files from the host with
`boxpool cp`. The guest destination must already be a directory, and a copy
can overwrite same-named files. Avoid broad home/project copies containing
credentials, `.env` files, dependency caches or unrelated uncommitted work.
There is no automatic synchronization in either direction.

On a Linux head using its local root-owned daemon, prefix sandbox commands with
`sudo /usr/local/bin/boxpool`. A Mac head or a user client configured with
`boxpool login` uses `boxpool` without sudo. See [client setup](guide.md#use-the-pool-from-a-client-laptop).
The CLI talks through the head and the worker's authenticated transport; this
workflow does not require SSH into the guest.

## Start the agent session

On your laptop, use the skill and give it the sandbox and **guest** project path:

> Use boxpool-workspace with sandbox agent-work, project /root/work/project.
> Inspect the project instructions, fix the failing tests, run the relevant
> tests/build there, and show me the diff. Keep project operations in Boxpool.

Start from a small local session directory if your harness requires a working
directory; it does not need a local project checkout. Personal harness skills
and settings remain local. The skill resolves the name with `boxpool ls --json`
and records its name and full ID. The current CLI targets names, so the skill
rechecks that identity before a mutation batch and after reconnecting; it stops
if the ID changed. This is a cooperative check, not an atomic identity binding.
It verifies the running state and guest directory, then reads the project's
instructions remotely.

Project tool calls should use commands such as:

```sh
# Substitute the sandbox name and actual guest path on every call.
boxpool exec SANDBOX_NAME -- sh -c 'cd "$1" && shift && exec "$@"' sh /root/work/project npm test
boxpool exec SANDBOX_NAME -- sh -c 'cd "$1" && shift && exec "$@"' sh /root/work/project git diff
```

The [skill source](../internal/skills/boxpool-workspace/SKILL.md) contains recipes
for reading, searching, creating and editing files with quoted scripts. Use
`exec -i` when sending a script via stdin. Builds/tests run in the guest; their
output returns to the local agent. A missing guest tool should be installed
there, within the task's scope, rather than run on the laptop as a substitute.

If the workspace stops or becomes unreachable, project work pauses. The skill
must not fall back to local project commands or silently provision a replacement.
Check remote state before repeating a write whose response was lost. Exiting
the local agent leaves the workspace running; reconnect to the same ID/path
later. `stop`, `start` and `rm` have their usual [persistence behavior](guide.md#resources-and-persistence).

## Verify a first session

1. Select a dedicated test sandbox and guest project path. Ask the agent to
   report the resolved ID, `pwd` and `uname -s`; expect the guest path and Linux.
2. Read the guest's project instructions, list files, and search for a known
   string. Check that calls use `boxpool exec`, including file reads/edits.
3. Create a small project file in the guest, make one precise edit, and inspect
   it again. Verify the host session directory has no project artifacts.
4. Run a known failing test, fix it remotely, then run the passing test/build.
   Inspect `git diff --check`, `git diff` and `git status --short` in the guest.
   Check exit codes and stderr, rather than assuming every command succeeded.
5. Stop only the dedicated test sandbox. A remote read must fail, with no local
   project fallback. Start it again and verify the same ID/path and files.
6. Remove only the disposable test sandbox when finished. Preserve a real work
   sandbox unless the user asks for cleanup.

See the [recorded Mac-to-Linux verification](benchmarks/local-agent-skill.md)
for the prototype's tested behavior and limits.
