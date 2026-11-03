# Sandbox isolation

What keeps a sandbox away from the host and from other sandboxes, per
backend, and where that stops. Every sandbox runs its commands as root
inside the guest, so a control the guest can change is not isolation.

## Threat model

Sandbox commands, network packets, file-transfer archives and sandbox web apps
are untrusted, including commands run as guest root. The assets protected are
host files and services, sibling sandbox state, pool credentials and access
tokens. The host administrator, local daemon socket clients, Docker Engine,
hypervisor, guest image supply chain and pool head are trusted. A compromised
head controls its nodes; giving someone the local API socket grants the
rights of its daemon user.

Host-enforced VM/container boundaries and network filters separate guests;
vsock or an inode-checked Unix socket carries the agent protocol. Remote API
requests cross a TLS and token-scope boundary, and exposed apps cross a proxy
and cookie boundary. A kernel exploit can escape a Docker container, which
shares the host kernel. LAN services on other machines remain reachable over
outbound NAT; boxpool does not impose an Internet-only egress policy.

## Compute

| Backend | Boundary |
| --- | --- |
| Firecracker (Linux) | A KVM microVM with its own kernel; Firecracker's seccomp filters, no jailer (see `internal/backend/firecracker/doc.go`) |
| Virtualization framework (macOS) | A VM with its own kernel, run by Apple's Virtualization framework in a per-VM XPC helper |
| Docker (Linux fallback) | A container sharing the host's kernel; weaker, and `doctor` warns about it |

The agent talks to the daemon over vsock (VMs) or a socket in the
sandbox's own directory (Docker), never over the network.

## Network

| | Firecracker | macOS 26+ | macOS 14–15 | Docker |
| --- | --- | --- | --- | --- |
| Outbound IPv4 | NAT, when the daemon runs as root | NAT | None: no network device | NAT |
| Sandbox to sandbox | Dropped by the host | Not forwarded by the host | None | Dropped by Docker's rules |
| Sandbox to the host's services | Dropped by the host | Dropped by the host, except gateway DHCP/DNS | None | Dropped by the host; embedded DNS exception below |
| Who enforces it | nftables on the host, needs root | Host packet bridge and macOS | No network | Host nftables and Docker Engine; needs root |

### Linux (Firecracker)

Each sandbox has its own tap device and /30. boxpool's nftables table
forwards only a sandbox's outbound traffic and the replies, and drops
everything else from taps: to other taps, to the host itself and with a
spoofed source address. The guest cannot change host rules.
`internal/backend/firecracker/doc.go` describes the ruleset and its limits.

### Linux (Docker fallback)

The daemon must run as root with `ip` and `nft` installed, using a local
rootful Docker Engine in the same network namespace. `doctor` can still
probe Engine access as a regular user; starting the Docker daemon backend
without root fails with instructions to use `sudo boxpool up`.

Every owned bridge gets a separate `inet bp_d_<hash>` nftables table before
its container is created or started. Rules are also restored before adopting
containers after a daemon restart. Network names and ownership labels must
match, and the Engine's default `br-<network ID>` must exist locally as a
bridge. Rootless engines and custom bridge names are refused. Failure to
restore isolation stops verified running containers rather than adopting them
unprotected. If Engine access itself fails, stopping them cannot be guaranteed;
stop them administratively before retrying the daemon.

The prerouting chain drops IPv6, spoofed IPv4 sources, and guest-initiated
traffic addressed to the host **before Docker DNAT**. This covers gateway,
LAN and VPN addresses, newly added host addresses, and published host ports.
INPUT drops guest-initiated host traffic even on an established connection.
Replies to connections initiated by the host are retained. FORWARD permits
guest egress only through the host's outbound interface and accepts inbound
reply traffic, keeping other bridges and Firecracker taps unreachable even
if Docker's own isolation rules change. NAT and DNS/HTTP egress remain usable.
The outbound interface is refreshed at Start; if routes change while guests
run, traffic on another uplink fails closed until Start is called again.

Docker's embedded resolver at `127.0.0.11` remains an infrastructure exception:
Docker may forward DNS to a host-loopback resolver from its own host namespace.
Guests can use that resolver for DNS queries; this is not a general socket
forwarder. Administrators requiring no host DNS access must configure Docker
with external upstream DNS servers. Ordinary guest connections to host port
53 are filtered like other host services.

Rules remain while the network exists, including while a sandbox is stopped
or cleanup fails. Successful network removal deletes only its own table.
Running containers survive daemon shutdown, so their rules must remain then.
Do not flush the tables while guests run. Before upgrading, stop existing
Docker sandboxes; the old daemon has no host-service isolation until the new
one installs the rules. Reusing their disks/volumes needs no migration.

### macOS

Networking needs macOS 26 or later, and no root or extra entitlement.

**macOS 26 and later.** Each sandbox has a vmnet shared network of its own
(a filtered `VZFileHandleNetworkDeviceAttachment` connected to a vmnet
packet interface): vmnet allocates a separate /24 for each
network dynamically (192.168.64.0/24 for the first), with a NAT for
outbound IPv4. The VM has one virtio network device on it, configures it
with the kernel's `ip=dhcp`, and uses the NAT's DNS proxy, the network's
gateway. macOS does not forward packets between those networks. A TCP
connection from sandbox A to sandbox B's address fails and B receives none
of A's packets, even when A routes them through its gateway explicitly.
The isolation is on the host, so root in a guest cannot undo it.

A sandbox keeps its network from its first Start until it is removed, so
it gets the same address after a stop and start, and no other sandbox can
take over its address while it is stopped. A network that a VM has used
keeps its /24 until the daemon exits, and a process can hold at most 32
networks, so boxpool never releases them: a removed sandbox's network goes
to a pool, and a new sandbox may take it 2 minutes after the removal, once
late replies to the removed sandbox's address have expired. So at most 32
sandboxes, running or stopped, plus those removed in the last 2 minutes,
can have networking at once; starting another fails with "at most 32
sandboxes can have networking at once on macOS". A daemon restart gives
sandboxes new networks, so their addresses can change.

**macOS 14 and 15, or a build with an SDK older than macOS 26's.**
Sandboxes have no network device, and the daemon log and `boxpool sh` say
so. The framework's shared NAT (`VZNATNetworkDeviceAttachment`) would put
all VMs on one /24. It does not forward frames between VMs, but macOS
routes packets between addresses on that network: root in sandbox A can
add a host route to B via the gateway, and A's packets then reach B. So
boxpool does not use it.

**Host-service isolation (a hard public-beta blocker, #106).** The VM's
only network attachment is a connected datagram socket. A packet bridge
in the daemon filters guest frames before passing them to
`vmnet_interface_start_with_network` on that sandbox's private network.
It needs neither root, a privileged helper, extra entitlements, nor PF
rules. There is no direct, unfiltered vmnet attachment or NAT fallback.
A setup or I/O failure disconnects the network; vsock remains separate.

The bridge drops IPv4 traffic to the Mac's own addresses, including the
vmnet gateway and its LAN and VPN addresses. It reads host interface
addresses for each packet, so new addresses apply to running guests and
existing flows. Address-lookup failures deny traffic. Loopback,
link-local, multicast, limited broadcasts and subnet network/broadcast
addresses are also denied. The only infrastructure exceptions are UDP
DHCP from port 68 to port 67 at the gateway or the limited broadcast, and
TCP/UDP DNS to port 53 at the gateway.

The bridge also checks the sandbox's source MAC and IPv4 subnet. It drops
IPv6 frames even if guest root configures IPv6 manually; NAT66 and router
advertisements are disabled. VLANs, other Ethernet protocols, IPv4 options
(including source routing), malformed IP headers and unsolicited host-bound
IPv4 fragments are not forwarded. A fragmented host reply is admitted only
when its first fragment matches an existing reply flow; up to 1024
fragment identities expire after 30 seconds. ARP is limited to the
sandbox's own network and cannot claim a host address. None of these checks depends on guest
firewall rules or on trusting the packet's source address.

The Mac can still initiate TCP, UDP and ICMP echo traffic to a sandbox.
Only matching replies gain an exception: a guest cannot create one by
sending a SYN or an unsolicited ACK. Reply state is per running VM,
bounded to 4096 flows, and expires after 24 hours of idle TCP or one minute
for UDP, ICMP and closing TCP. Pause and restart discard that state;
reconnect host-initiated sessions after restoring a VM. Direct guest
connections to host services remain denied.

The filter and its sockets are removed when the VM stops or is removed;
`boxpool down` stops the VMs and closes their bridges. Nothing is installed
in the system firewall, so uninstall needs no privileged cleanup. Cached
warm bases from the old attachment are rebuilt automatically.

**Upgrade from the direct vmnet attachment:** cleanly stop existing macOS
sandboxes with `boxpool stop <name>` before installing this version,
including paused ones. Saved NIC state from the old attachment cannot
resume networking with the new socket attachment. If already upgraded,
run `boxpool stop <name>` and then `boxpool start <name>`: Stop restores
its saved memory over vsock and flushes the disk, and Start cold-boots the
filtered NIC. Disk data is retained; in-memory processes do not survive the stop. Do not
delete `machine.state` to migrate, since it can contain dirty disk data.

Sandboxes on macOS 14–15 still have no NIC. Bridged networking needs the
`com.apple.vm.networking` entitlement and is not used.

### Why not filter in the guest

Rules applied inside the guest (for example nftables loaded by the agent
at boot) would need a new guest image, since the image has no `nft` or
`iptables` binary, and root in the guest could remove them. The macOS 26
per-sandbox networks and host packet bridge enforce isolation where the
guest cannot reach it, so boxpool does not filter in the guest.

### How this was checked

`TestIntegrationHostServices` binds TCP and UDP listeners on every IPv4
interface and checks the gateway and all current non-loopback host addresses
(including connected VPNs) from cold, resumed and warm-cloned guests. It
also sends UDP with a source outside the sandbox's subnet. No connection
or datagram may reach the listeners, while DNS over UDP and TCP and an
outbound HTTP request by name must work. `TestIntegrationHostIPv6` injects
raw IPv6 UDP frames addressed to the Mac, without relying on guest IPv6
configuration, and checks a wildcard IPv6 listener receives none.
`TestIntegrationHostReplies` checks host-initiated UDP and ICMP echo,
including 4096-byte fragmented requests and replies.
`filter_test.go` covers packet formats, source spoofing, live address
changes, lookup failures, broadcasts, fragment/option bypasses, bounded
reply state and expiry; it also supplies a packet-parser fuzz target.

Manual confirmation, on macOS 26+ with a signed development build:

1. Run `python3 -m http.server 8000 --bind 0.0.0.0` on the Mac.
2. In `boxpool sh`, find the gateway with `cat /etc/resolv.conf`. Run
   `timeout 3 bash -c 'exec 3<>/dev/tcp/<gateway>/8000'`, then repeat with
   the Mac's LAN and VPN IPv4 addresses. Each must fail and the HTTP server
   must receive no request.
3. In that guest run
   `bash -c 'exec 3<>/dev/tcp/example.com/80; printf "HEAD / HTTP/1.0\r\nHost: example.com\r\n\r\n" >&3; timeout 10 head -n 1 <&3'`.
   Expect an HTTP status line. Pause/resume and repeat the probes.
4. Run `boxpool down`. The daemon and VM helpers must stop; no PF anchor or
   privileged helper has been installed to remove.

`TestIntegrationNetwork` in `internal/backend/vz` fails, rather than
skips, on macOS 26 or later when per-sandbox networks are off. It boots
sandboxes A and B, checks that they are on different networks, fetches
`http://example.com/` from A with bash's `/dev/tcp` (the image has no
curl), checks that the Mac can reach a listener in B, and that A cannot,
directly or after adding a host route to B through its gateway (with
`SIOCADDRT`, as the image has no `ip`), with B's TCP `InSegs` counter
unchanged. It then stops A and starts C: C is on another network, and
with A's address added to its own interface and a listener up, the Mac
still cannot reach it at A's address; A gets its address back when it
starts again. `TestIntegrationNoNetwork` checks that without per-sandbox
networks a sandbox has only `lo`, and `TestIntegrationNetworkLimit` the
32-network limit and the 2-minute pool quarantine. The route probe
delivered A's segments to B on the shared NAT (measured on macOS 27, not
on macOS 14 or 15 themselves), which is why older macOS gets no network.

## Pool

A daemon that has not joined another pool is a head: it keeps a CA
(`ca.pem`, `ca-key.pem`, mode 0600) in its state directory and serves the
pool API over TLS 1.3 on TCP port 7443, on every interface. If the port is
taken, the daemon logs a warning and runs without it.

- `POST /v1/nodes` is the only endpoint open without a client
  certificate. It needs an unused, unexpired join token
  (`boxpool token create --join`); the head stores only the SHA-256 of its
  secret.
- Every other endpoint needs a client certificate signed by the head CA.
  A certificate from another CA, or an expired one, fails the handshake.
- A joining node makes its key itself and sends only a certificate
  request. It trusts the head only if the head's chain contains the CA
  whose SHA-256 is in the token, so a token pins the head.
- A node dials the head; the head dials a node only for raft once it is a
  replica (below). The node must reach the head directly, over a LAN or a
  tailnet. The node reports to the head every 5 seconds
  (`POST /v1/heartbeat`) and keeps four requests to `GET /v1/tunnel`
  waiting there; the head answers one with 101 to send the node a request
  for one of its sandboxes over that connection.
- The head can therefore send a node any request of the node's local API,
  as a local user of the node could: the head is trusted with every node.
- A machine cannot join its own pool or join another pool while it has
  nodes. While joining, it refuses registrations and token creation.
  Once joined, its old tokens cannot register nodes, even before the old
  head listener closes on restart.

### Exposed ports

`boxpool expose` serves a sandbox port on the head at
`https://<head's tailnet name>:<port>/`. tailscale serve terminates TLS
with a certificate for that name and forwards to the head's daemon on
`127.0.0.1:<port>`, so the URL is reachable only on the tailnet, and no
plain-HTTP port listens on other interfaces. The head reaches the port
through the node's tunnel and the guest agent's `GET /v1/tcp`; the guest
needs no open port.

- Unless `--public`, every request needs an `expose` access token scoped
  to that sandbox: `?boxpool_token=` in the link `expose` prints, which
  the head swaps for an `HttpOnly`, `Secure`, `SameSite=Lax` cookie and
  redirects without, or that cookie. Otherwise the answer is 401.
- Every exposed URL shares the head's host name; cookies do not distinguish
  ports. The head strips all browser cookies except authenticated app-cookie
  envelopes belonging to that expose. Backend `Set-Cookie` values are
  encrypted with AES-GCM, namespaced per expose and made host-only, `Secure`
  and `HttpOnly`, including on WebSocket handshakes. Only their original
  app names and values reach the corresponding app. Keys are derived from
  the head's private CA key and the expose's identity and creation time:
  sessions survive daemon restarts but cannot move to another expose or a
  later expose reusing its port. Paths and lifetimes are preserved; changing
  the name or value of an envelope invalidates it. Legacy raw app cookies
  are ignored, so existing sessions need a new login. JavaScript cookie
  APIs are unsupported: readable envelopes could be stolen and replayed
  by JavaScript on a sibling port.
- Pages on two exposed ports are different origins but the same site, so
  one sandbox's page can send requests, with cookies, to another's URL
  (it cannot read the answers unless that app allows it).
- For ordinary app exposes, the token is checked when each request starts;
  a WebSocket already open when the token is revoked stays open until it
  closes. Desktop viewers also recheck their token every two seconds and
  disconnect on revocation or expiry. Removing either expose or stopping
  the daemon cancels its requests and open WebSockets.
- Only an Origin matching that expose's HTTPS URL is translated to the
  guest's localhost Origin. Foreign origins remain foreign, so backend
  origin checks still reject cross-origin WebSockets and form requests.
- Answers from a sandbox's port are never read as sandbox records when
  the head forwards them from a node.

### Audit log

The head appends successful `sandbox.create`, `sandbox.rm`, `sandbox.exec`,
`sandbox.expose`, `sandbox.unexpose`, `token.create`, `token.revoke` and
`login` events to `audit_events` in its private SQLite database. Forwarded
actions are recorded once on the head with the originating access token ID.
Creating a private expose also records its token creation; removing it
records a token revocation only when it actually revokes the token.
Join-token targets use the first 12 characters of the secret's
hash, as access-token IDs do. A login means the head accepted the CLI's
explicit `POST /v1/login`, rather than a health check.

Exec records the successful WebSocket upgrade, before any commands run,
including interactive shells. It records the sandbox name, not the command
or its output. Reusing an existing sandbox does not record another create.
Failed and denied operations are not successful-action audit events.

Each authorized exposed-URL request records `sandbox.access` before the
redirect or proxy starts, even if the app later returns an error. This
includes the token-to-cookie redirect and WebSocket handshakes. Public URLs
use `public` as the actor; local socket operations use `local`, without
distinguishing users sharing that socket. Targets contain only sandbox
names, sandbox ports or token IDs. Token secrets, command text, cookies,
app paths and URL queries are never stored in the audit table.

`GET /v1/audit` and `boxpool audit` are local to the head. Remote scoped
tokens cannot read the log. The API returns newest first, defaults to 100
events, allows `limit=1` through `1000`, and accepts `before=<event ID>` for
older pages. There is no event update or individual-delete API. Rows older
than 90 days are pruned on append and read; idle databases are pruned on the
next audit operation. Token revocation and sandbox deletion preserve their
history. Events are not replicated, so a promoted head starts its own log.

Mutation responses are acknowledged only after the audit transaction
commits. If an action finishes but the audit write fails, the API returns
500 with "operation completed but audit failed" and logs the failure; the
action and audit cannot be atomic across hypervisors or node tunnels.
Exec upgrades and exposed requests stop if their audit write fails. A crash
between an action and its audit write can leave an unrecorded action. This
is an activity history, not a tamper-proof log against the host owner, who
can edit the database directly.

### Replicas and failover

From the pool's second machine on, the head replicates its state (nodes,
their last reports, and join tokens) with raft to up to three machines,
itself included: always-on nodes first, then the oldest. With three or
more machines the replicas vote and elect a new head when the head goes;
with two, the second keeps a copy without a vote and `boxpool promote`
makes it the head.

- The head gives each replica the head CA's key, over the node's tunnel,
  only if it matches the CA the node joined with. A replica is therefore
  trusted as the head is, and serves port 7443 with the same CA.
- Raft runs on port 7443: a TLS connection that negotiates the ALPN
  protocol `boxpool-raft`. Each side presents its node certificate. A
  replica accepts raft only from a node in its raft configuration, or,
  before it has one, from the head that made it a replica. Only a holder
  of the CA serves the head's name, so a node without the key cannot pose
  as a replica.
- The head dials replicas for raft, so replicas need port 7443 reachable
  from the other replicas. Other nodes still only dial out.
- A node keeps the replicas' addresses (`pool/peers`) and, when the head
  stops answering, reports to the first one that answers as head.
  Replicas use Raft's elected leader for their reports and tunnels, so a
  returning head can follow it even without saved peers.
- The head commits its local sandbox inventory through Raft before
  acknowledging a create, start, stop, pause, resume or delete. If it loses
  leadership during the operation, the local change may have happened,
  but the request fails rather than acknowledge an unreplicated change.
- `boxpool promote` refuses while it can reach any other replica. An old
  head that comes back after a promotion asks the other replicas, finds
  one leading without it, drops its raft state, and follows that head as
  a node. The promoted head does not accept its raft connections meanwhile.

Limits: node certificates last as long as the CA (10 years) and cannot be
revoked yet; a replica keeps the CA key after it stops being one. Anyone
who can reach port 7443 can attempt a handshake; restrict it with a host
firewall if the network is untrusted. Join tokens name the head that
issued them, so a token from before a failover does not reach the new
head; issue a new one there.

## Hardening checks (#53)

`test/security/namespaces.sh` and `host-network.sh` are embedded by a shared
host harness and executed as guest root by the Docker and Firecracker Linux
integration jobs. They check host sockets/devices and host-filesystem markers,
privileged Docker mount/raw-socket attempts, capability/seccomp settings,
proc/sys exposure, TCP and UDP traffic to all current host IPv4 addresses and
gateways, plus DNS and HTTP positive controls. VM root can legitimately mount
and use raw sockets inside its own kernel; VM checks assert a different kernel
boot ID and no access to the host marker instead. Docker also tests peer
isolation, host-initiated TCP, restart adoption and the Unix-socket symlink
attack. macOS's existing host-network integration and packet fuzz tests cover
its separate host packet bridge.

`FuzzFileUploadPaths` exercises the agent HTTP upload handler with traversal,
absolute names and symlink targets. File contents, removals, symlinks and
metadata use the opened destination directory, even if its name is replaced
concurrently; `TestExtractTarDestinationReplaced` checks that regression for
files, directories and symlinks. Extraction preserves normal modes, nanosecond
timestamps and dangling symlinks. Downloads may include arbitrary symlink
targets as links, but extraction never follows them outside its destination.

The #53 source review also covered exposed-port routing, origin translation,
app-cookie isolation, token parsing/scopes and revocation. Existing regression
tests cover those controls. Exposed apps share a browser site: apps must defend
against CSRF from sibling ports, and an already-open exposed WebSocket remains
open after token revocation, as described above. These are documented v1 trust
limits, not guarantees provided by guest network filtering. This review is not
the external audit planned for M7.

### Desktop viewer

Desktop URLs reserve non-TCP port 0 in the expose store and always require
expose authentication. The proxy rejects foreign Origins before serving
the viewer, its control API or WebSocket. VNC and X listen only on guest
Unix sockets; RFB travels over the existing private agent channel, never
a guest-network listener. Embedded noVNC assets need no CDN.

TigerVNC disables key and pointer events until takeover; clipboard transfer
and client display resizing stay disabled. Only one viewer is supported.
Release or disconnect disables input again; a failure to reset input stops
the desktop. The `human_control` API flag asks agents to pause automation
cooperatively and does not stop arbitrary guest processes. Chromium runs
with `--no-sandbox` inside the existing VM/container boundary, including
the capability-free Docker fallback. See [desktop](desktop.md) for setup,
limitations and real-device acceptance steps.
