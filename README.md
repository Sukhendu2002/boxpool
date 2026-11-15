# Boxpool

**Your machines. One workspace pool.**

Run isolated Linux workspaces on your own Apple silicon Macs and Linux
machines. Start on one machine, then add spare hardware when you need more
capacity. Keep your coding agent on your laptop, or use the agents bundled
inside a workspace.

[Get started](docs/getting-started.md) · [Documentation](https://boxpool.run/docs/) ·
[Contribute](CONTRIBUTING.md) · [Roadmap](https://github.com/Sukhendu2002/boxpool/issues/64)

## A workspace in your terminal

Install on a supported host:

```sh
curl -fsSL https://boxpool.run/install | sh
```

If `boxpool` is not found, add the directory printed by the installer to your
[PATH](docs/installation.md#shell-installer).

**On an Apple silicon Mac:**

```sh
boxpool doctor
boxpool up
boxpool sh --name hello --cpu 1 --mem 512m --disk 3g
```

**On a Linux host:**

Install the [host prerequisites](docs/installation.md#linux-prerequisites), then:

```sh
sudo "$(command -v boxpool)" doctor
sudo "$(command -v boxpool)" up
sudo /usr/local/bin/boxpool sh --name hello --cpu 1 --mem 512m --disk 3g
```

**Inside the workspace:**

```sh
printf 'hello from a workspace\n' > /root/hello.txt
cat /root/hello.txt
exit
```

Exiting the shell keeps the workspace running and its files intact. Follow the
[first workspace guide](docs/first-workspace.md) to reconnect, copy files and
clean up. Run `boxpool` commands on the host, outside the workspace.

## What you can do

- **Give an agent a separate workspace.** Keep Pi, Codex or Claude Code and
  your model login on your laptop; use the [local-agent workflow](docs/local-agents.md)
  for project files, builds and tests. The skill installer is unreleased;
  its guide includes source-build instructions.
- **Use more of your hardware.** [Add another machine](docs/pool.md), label it
  and choose where a workspace runs. Guests are always Linux, including on Macs.
- **Keep work between sessions.** Stop and start a workspace, or clone its disk
  with [snapshots](docs/guide.md#snapshots). Optionally share
  [app previews](docs/expose.md) and [desktops](docs/desktop.md) over your tailnet.

## Requirements and status

Early releases are available; Boxpool is under active development.

| Host | Support |
| --- | --- |
| Apple silicon Mac | macOS 14+, APFS; **macOS 26+ for workspace internet access** |
| Linux amd64 / arm64 | Kernel 5.10+, systemd, root access; KVM for Firecracker |
| Linux without KVM | Rootful Docker fallback, with weaker isolation |
| Intel Mac / Windows | Unsupported as hosts |

Hosts need **20 GB available disk space**, plus CPU and RAM for workspaces.
Mac releases are ad-hoc signed, not notarized. See [installation](docs/installation.md)
for prerequisites, Homebrew, npm and source builds. Read the
[trust model](docs/security.md#trust-at-a-glance) before running untrusted workloads.

## Contributing

Start with [CONTRIBUTING.md](CONTRIBUTING.md) for setup, a repository map and
checks. `make check` runs the ordinary checks without starting a daemon or VM.
[AGENTS.md](AGENTS.md) contains the shared contribution rules.

[Documentation index](docs/README.md) · [Report a bug](https://github.com/Sukhendu2002/boxpool/issues/new/choose)

Apache 2.0. See [LICENSE](LICENSE).
