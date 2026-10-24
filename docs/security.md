# Sandbox isolation

What keeps a sandbox away from the host and from other sandboxes, per
backend, and where that stops. Every sandbox runs its commands as root
inside the guest, so a control the guest can change is not isolation.

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
| Outbound IPv4 | NAT, when the daemon runs as root | NAT | NAT | NAT |
| Sandbox to sandbox | Dropped by the host | Not forwarded by the host | One-way, see below | Dropped by Docker's rules |
| Sandbox to the host's services | Dropped by the host | Reachable | Reachable | Reachable (#53) |
| Who enforces it | nftables on the host, needs root | macOS | macOS | Docker Engine |

### Linux (Firecracker)

Each sandbox has its own tap device and /30. boxpool's nftables table
forwards only a sandbox's outbound traffic and the replies, and drops
everything else from taps: to other taps, to the host itself and with a
spoofed source address. The guest cannot change host rules.
`internal/backend/firecracker/doc.go` describes the ruleset and its limits.

### macOS

Networking needs no root or extra entitlement, so it is on by default. Each
VM has one virtio network device on a NAT, configures it with the kernel's
`ip=dhcp`, and uses the NAT's DNS proxy (the gateway, 192.168.64.1 on the
first network).

**macOS 26 and later.** Each running sandbox has a vmnet shared network of
its own (`VZVmnetNetworkDeviceAttachment`, one /24 in 192.168.64.0/19), and
macOS does not forward packets between those networks. A TCP connection
from sandbox A to sandbox B's address fails and B receives none of A's
packets, even when A routes them through its gateway explicitly. The
isolation is on the host, so root in a guest cannot undo it. A process can
hold at most 32 such networks, so at most 32 sandboxes run at once. A
network is reused once its sandbox stops.

**macOS 14 and 15, or a build with an SDK older than macOS 26's.** All VMs
share the framework's NAT (`VZNATNetworkDeviceAttachment`) on one /24. The
NAT does not forward frames between VMs, so ARP for another sandbox fails
and a normal connection to it gets "No route to host". But macOS routes
packets between addresses on that network: root in sandbox A can add a
host route to B via the gateway, and A's packets then reach B. B's replies
go straight to A over the blocked link and are lost, so TCP connections do
not complete, but A can still deliver one-way traffic (UDP datagrams, TCP
SYNs) to B. Use macOS 26 or later where this matters.

**Limits on every macOS version.**

- A sandbox can connect to services on the Mac that listen on all
  interfaces, through the gateway or the Mac's LAN and VPN addresses.
  Services bound to 127.0.0.1 are out of reach. The Linux backend drops
  this traffic on the host. Blocking it on macOS needs a host packet filter
  (pf, which needs root) and is not done.
- The Mac can connect to a sandbox's address, as on Linux.
- Bridged networking needs the `com.apple.vm.networking` entitlement and is
  not used.

### Why not filter in the guest

Rules applied inside the guest (for example nftables loaded by the agent
at boot) would need a new guest image, since the image has no `nft` or
`iptables` binary, and root in the guest could remove them. The macOS 26
per-sandbox networks enforce isolation where the guest cannot reach it,
so boxpool does not filter in the guest.

### How this was checked

`TestIntegrationNetwork` in `internal/backend/vz` boots two sandboxes,
fetches `http://example.com/` from A with bash's `/dev/tcp` (the image has
no curl), checks that the Mac can reach a listener in B, and that A cannot,
with B's TCP `InSegs` counter unchanged. The macOS 14–15 behaviour above was
measured with the shared NAT on macOS 27, not on macOS 14 or 15 themselves,
by adding a static ARP entry in A (no segments reached B) and then a host
route through the gateway (they did).
