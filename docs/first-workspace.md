# Your first workspace

Start with [installation and host checks](installation.md). This guide uses
one machine and a small disposable workspace: 1 CPU, 512 MiB RAM and 3 GiB disk.
Give real projects more resources as needed. Defaults are 2 CPUs, 2 GiB RAM
and 10 GiB disk; resource flags apply when a workspace is first created.

## Start the service

**On your Mac:**

```sh
boxpool up
boxpool ls --nodes
```

**On your Linux host:**

```sh
sudo "$(command -v boxpool)" up
sudo /usr/local/bin/boxpool ls --nodes
```

`up` installs and starts the daemon, downloading the pinned image on first
start. `ls --nodes` should show one `ready` head. The head coordinates the
pool and can run workspaces itself. Keep using `sudo /usr/local/bin/boxpool`
for local daemon commands on Linux; on macOS use `boxpool` without sudo.

## Open a shell

Use an interactive terminal. If you already have a workspace called `hello`,
choose a different name to avoid reusing it.

**On your Mac:**

```sh
boxpool sh --name hello --cpu 1 --mem 512m --disk 3g
```

**On your Linux host:**

```sh
sudo /usr/local/bin/boxpool sh --name hello --cpu 1 --mem 512m --disk 3g
```

You should see a `root@hello` prompt. **Inside the workspace:**

```sh
uname -s
printf 'hello from a workspace\n' > /root/hello.txt
cat /root/hello.txt
exit
```

Expect `Linux` and `hello from a workspace`. An empty `ls` in `/root` is
normal for a new workspace. `exit` returns to the host; the workspace stays
running. Run the following commands at your host prompt, not `root@hello`.

## Reconnect and run commands

**On your Mac:**

```sh
boxpool exec hello -- cat /root/hello.txt
boxpool ls
boxpool sh hello
```

**On your Linux host:**

```sh
sudo /usr/local/bin/boxpool exec hello -- cat /root/hello.txt
sudo /usr/local/bin/boxpool ls
sudo /usr/local/bin/boxpool sh hello
```

Expect the same text, and `hello` in state `running`. After reconnecting,
run `exit` in the workspace to return to the host again.
`sh hello` attaches to an existing workspace and refuses a misspelled name;
`sh --name hello` can create one. Use `exec` for scripts and command exit codes.

## Copy a file

Create a small file on your host, then copy it into the workspace's existing
`/root/` directory.

**On your Mac:**

```sh
printf 'from the host\n' > /tmp/boxpool-hello.txt
boxpool cp /tmp/boxpool-hello.txt hello:/root/
boxpool exec hello -- cat /root/boxpool-hello.txt
```

**On your Linux host:**

```sh
printf 'from the host\n' > /tmp/boxpool-hello.txt
sudo /usr/local/bin/boxpool cp /tmp/boxpool-hello.txt hello:/root/
sudo /usr/local/bin/boxpool exec hello -- cat /root/boxpool-hello.txt
```

Expect `from the host`. There is no automatic host/guest file synchronization.
See [commands and files](guide.md#shells-commands-and-files) for stdin, copying
out, permissions and bundled coding agents.

## Stop or clean up

To keep your files, stop the workspace and start it later.

**On your Mac:**

```sh
boxpool stop hello
boxpool start hello
boxpool exec hello -- cat /root/hello.txt
```

**On your Linux host:**

```sh
sudo /usr/local/bin/boxpool stop hello
sudo /usr/local/bin/boxpool start hello
sudo /usr/local/bin/boxpool exec hello -- cat /root/hello.txt
```

Expect the saved text after starting it again. VM disks persist; Docker
fallback only preserves `/root` across stop/start. Stopped workspaces still
reserve capacity. **When finished with this disposable example**, delete it:

**On your Mac:**

```sh
boxpool stop hello
boxpool rm hello
```

**On your Linux host:**

```sh
sudo /usr/local/bin/boxpool stop hello
sudo /usr/local/bin/boxpool rm hello
```

`rm` deletes that workspace's disk. To stop the node's daemon, use
`boxpool down` on Mac or `sudo /usr/local/bin/boxpool down` on Linux.
Linux workspaces keep running after `down`; Mac VMs stop. Stop workspaces
explicitly first if you want them all stopped. State remains for `up`.

Next: [use your local agent](local-agents.md), learn about
[persistence and snapshots](guide.md#resources-and-persistence), or
[add another machine](pool.md).
