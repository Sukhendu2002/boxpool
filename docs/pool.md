# Add another machine

First complete a [single-machine session](first-workspace.md). Your first
machine is the **head**, which coordinates the pool. Another machine is a
**worker**, contributing capacity. You need terminal access to both; SSH is
fine for setup. Runtime workspace commands use Boxpool's authenticated
transport, not SSH.

## Prepare the worker

[Install Boxpool and its host prerequisites](installation.md) on the second
machine. Each host needs at least 20 GB available disk space and enough CPU
and RAM for its own workspaces. Guests are Linux even on Mac hosts.

The worker must reach the head on **TCP 7443**. Replicas also need to accept
connections from pool peers on that port. Allow it between the two hosts
in their firewalls. For a LAN setup, use the head's LAN IP reachable from
the worker. The commands below use `192.168.1.10`; replace it with yours.

## Create a join token

**On a Mac head:**

```sh
boxpool token create --join --addr 192.168.1.10:7443
```

**On a Linux head:**

```sh
sudo /usr/local/bin/boxpool token create --join --addr 192.168.1.10:7443
```

Copy the **whole** token starting with `bpjoin`. Keep it private: it is a
secret, single-use and expires after one hour by default.

### Across different networks

Connect both machines to the same Tailscale or Headscale tailnet using the
system Tailscale client first. Userspace-only Tailscale is unsupported.
Use this token command instead of the LAN one:

**On a Mac head:**

```sh
boxpool token create --join --transport tailscale
```

**On a Linux head:**

```sh
sudo /usr/local/bin/boxpool token create --join --transport tailscale
```

Allow TCP 7443 between peers in tailnet policy and host firewalls.
No router port forwarding is needed. Joining does not require HTTPS
certificates; [app previews](expose.md) have additional requirements.

## Join and label the worker

Replace `PASTE_JOIN_TOKEN` with the token. `join` starts the worker daemon
itself; do not start a separate pool with `up` first.

**On a Mac worker:**

```sh
boxpool doctor
boxpool join 'PASTE_JOIN_TOKEN'
boxpool node label worker
```

**On a Linux worker:**

```sh
sudo "$(command -v boxpool)" doctor
sudo "$(command -v boxpool)" join 'PASTE_JOIN_TOKEN'
sudo /usr/local/bin/boxpool node label worker
```

Expect `joined the pool at ... as node ...`. `worker` is a label you set,
not a special role. Labels are comma-separated selectors and are configured
locally on the machine they describe.

After about five seconds, **on a Mac head**:

```sh
boxpool ls --nodes
```

**On a Linux head:**

```sh
sudo /usr/local/bin/boxpool ls --nodes
```

Expect two `ready` nodes. The second may have role `replica`; it still runs
workspaces. See [join troubleshooting](troubleshooting.md#join-or-a-node-is-unreachable)
if the head cannot be reached or a token expires.

## Choose the worker

**On a Mac head:**

```sh
boxpool sh --node worker --name pool-hello --cpu 1 --mem 512m --disk 3g
```

**On a Linux head:**

```sh
sudo /usr/local/bin/boxpool sh --node worker --name pool-hello --cpu 1 --mem 512m --disk 3g
```

`--node worker` matches the label you just configured. Use your own label
if you chose something else. With no selector, Boxpool chooses a node with
enough free admission capacity. An existing workspace keeps its original node.

**Inside the workspace:**

```sh
uname -s
printf 'running on the worker\n' > /root/pool-hello.txt
exit
```

**On a Mac head:**

```sh
boxpool exec pool-hello -- cat /root/pool-hello.txt
boxpool ls
```

**On a Linux head:**

```sh
sudo /usr/local/bin/boxpool exec pool-hello -- cat /root/pool-hello.txt
sudo /usr/local/bin/boxpool ls
```

Expect the saved text and a running workspace owned by the worker. When done,
use `boxpool stop pool-hello` then `boxpool rm pool-hello` on the Mac head,
or the same commands with `sudo /usr/local/bin/boxpool` on a Linux head.
Removal deletes its disk.

Next: [placement and pool health](guide.md#placement-and-pool-health),
[client laptops](guide.md#use-the-pool-from-a-client-laptop),
[app previews](expose.md) or [desktop takeover](desktop.md).
