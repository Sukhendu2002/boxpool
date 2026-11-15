# Get started

Boxpool runs Linux workspaces on hardware you own. A workspace is a sandbox
with its own files, processes and resource budget. Use it for project work,
builds, tests or a coding agent, while keeping your host environment separate.

You only need **one supported machine** to begin. A second machine, Tailscale,
app previews and a desktop are optional.

## Your first session

1. [Install Boxpool](installation.md). Choose the commands for your host OS
   and run `doctor` to check it.
2. [Create your first workspace](first-workspace.md). Start the local service,
   open a shell, write a file, reconnect and clean up.
3. Choose how you want to work:
   - [Keep your agent on your laptop](local-agents.md), including its model
     login and skills, and run project operations through Boxpool.
   - [Run a bundled agent inside a workspace](guide.md#shells-commands-and-files)
     and authenticate it there.
4. [Add another machine](pool.md) when you want more capacity.

## Know where commands run

The docs distinguish three places:

| Location | What runs there |
| --- | --- |
| Mac host | `boxpool` commands, your local agent and its existing settings |
| Linux host | `sudo /usr/local/bin/boxpool` for the local root-owned daemon |
| Inside workspace | Linux project commands such as `npm test`, `git diff`, or `cat` |

The CLI calls a workspace a **sandbox** in output and command help. These are
the same thing. `boxpool` is a host tool; it is not installed inside the guest.
Even on a Mac, the workspace runs Linux. `--os macos` selects a Mac host,
not a macOS guest.

## Before you use it for real work

Boxpool is under active development. Back up valuable work, understand
[what persists](guide.md#resources-and-persistence), and read the
[security and trust model](security.md#trust-at-a-glance). Linux Docker
fallback shares the host kernel and provides weaker isolation than a VM.

The site documents the main branch. Features that are not in published v0.1.4,
such as the local-agent skill installer, are marked in their guides.
Use `boxpool version` to check your installation and `boxpool --help` for the
commands it supports.
