# Cross-OS pool join verification

Measured on 2026-10-29 for issue #35 and PR #120, source commit
`e1ae842721aa091241f94c385b762f2ea572cc7c`.

Both cross-OS joins passed over the machines' existing Tailscale tailnet.
Each complete `boxpool join` command finished in under 120 seconds:

| Joining node | Head | Complete command | Result |
| --- | --- | ---: | --- |
| macOS arm64, Apple M2 | Ubuntu amd64, Firecracker | 9.630059 s | PASS |
| Ubuntu amd64, Firecracker | macOS arm64, Apple M2 | 27.992531 s | PASS |

These are one sample per direction, not latency percentiles. The
[raw observations](m4-join/real-hosts.json) contain command output,
membership responses, binary checksums, and restoration checks. Join
tokens and private keys are excluded.

## Machines and build

- Mac: `Sukhendus-MacBook-Air.local`, macOS 27.0.1, Apple M2, arm64,
  Virtualization framework backend.
- Linux: `sukhendu-pc`, Ubuntu 24.04.3 LTS, kernel `7.0.0-38-generic`,
  Intel Core i5-1235U, amd64, Firecracker backend, real bare-metal host.
- Both binaries came from the same source commit using Go 1.26.3 on the
  Mac. The Linux binary was cross-compiled with `CGO_ENABLED=0`.

Build commands, before timing:

```sh
go build -ldflags '-X main.version=pr120-e1ae842' -o bin/boxpool ./cmd/boxpool
scripts/dev-sign.sh bin/boxpool
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -ldflags '-X main.version=pr120-e1ae842' \
  -o bin/boxpool-linux-amd64 ./cmd/boxpool
```

## Procedure and observations

1. Back up both service configurations and Linux's installed binary;
   record the Mac's original binary hash and leave that binary untouched.
   The Mac's original launch agent was running with no sandboxes; the Linux
   service was inactive and disabled. Preserve their original state
   directories, using fresh directories for every joining node.
2. Start the Linux head from the tested binary with a scratch
   `BOXPOOL_HOME`. Its existing artifact cache can be reused because
   preparing the head is outside the joining-node timer. Create the token:

   ```sh
   BOXPOOL_HOME=<linux-head-state> <linux-binary> token create --join \
     --ttl 15m --addr 100.90.107.72:7443
   ```

3. On the Mac, stop the idle original launch agent and time the actual
   CLI command with Python `time.monotonic`, from process creation until
   exit:

   ```sh
   BOXPOOL_HOME=<fresh-mac-state> bin/boxpool join <token>
   ```

   It exited 0 after **9.630059 seconds**. The command installed its
   binary and launch agent, downloaded the guest artifacts into the
   empty state directory, started the daemon, and joined as node
   `611d97d8f91e`. Startup reported 9.2 s.

4. `boxpool ls --nodes --json` on the Linux head listed the real Linux
   head and the Mac node. Independently request `GET /v1/nodes` from the
   Mac using its saved client certificate and private key, verifying
   the server with its saved CA. `curl --resolve` preserves the verified
   certificate name `boxpool-head` while connecting to the tailnet IP.
   The authenticated request returned both machines. The same request
   without a client certificate returned HTTP 401. The saved private
   key's permissions were 0600.
5. Stop the test Mac launch agent and Linux head. Start a Mac head with
   another scratch state directory, reusing the downloaded Mac artifacts.
   Create a token naming `100.83.148.12:7443`.
6. Temporarily move Linux's original `/var/lib/boxpool` aside and create
   an empty replacement. On Linux, time the complete root CLI command:

   ```sh
   sudo -n <copied-linux-binary> join <token>
   ```

   It exited 0 after **27.992531 seconds**. The command installed its
   binary, enabled the real systemd service, downloaded Firecracker and
   guest artifacts into the empty state directory, started the daemon,
   and joined as node `3231d591040c`. Startup reported 27.6 s.
7. The Mac head listed itself and the real Linux node. An independent
   certificate-authenticated `GET /v1/nodes` from Linux returned both
   machines, verifying the head with the saved CA. The Linux node's
   private key also had permissions 0600.
8. Stop all test services and foreground heads. Restore Linux's original
   state directory, binary, and systemd unit; verify matching file hashes,
   the original state directory inode, and inactive/disabled status.
   Restore the Mac's original plist and running launch agent; verify
   matching plist bytes and binary hash, the original program, and no
   sandboxes.

The timer includes preflight, installation, fresh artifact downloads,
daemon startup, registration, mTLS verification, and saving credentials.
It excludes building/signing the binaries, preparing the head, independent
post-command checks, and cleanup. This used the full CLI and each OS's
real service manager, not direct calls to the local join endpoint.

## Transport limitation

An earlier fresh Mac CLI attempt against the Linux private-LAN address
`192.168.0.171:7443` started its daemon but failed with `no route to host`.
SSH and foreground TCP probes could reach the Linux machine, and both
host firewalls were disabled. The cause of the background daemon's LAN
failure was not established; no firewall rules or privacy settings were
changed.

The passing measurements therefore verify the existing-tailnet transport
explicitly allowed by issue #35. Direct private-LAN joining remains
unverified on this Mac. This report covers issue #35's join criteria;
it does not certify the complete M4 gate (#43).
