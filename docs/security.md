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
| Outbound IPv4 | NAT, when the daemon runs as root | NAT | None: no network device | NAT |
| Sandbox to sandbox | Dropped by the host | Not forwarded by the host | None | Dropped by Docker's rules |
| Sandbox to the host's services | Dropped by the host | Reachable | None | Reachable (#53) |
| Who enforces it | nftables on the host, needs root | macOS | No network | Docker Engine |

### Linux (Firecracker)

Each sandbox has its own tap device and /30. boxpool's nftables table
forwards only a sandbox's outbound traffic and the replies, and drops
everything else from taps: to other taps, to the host itself and with a
spoofed source address. The guest cannot change host rules.
`internal/backend/firecracker/doc.go` describes the ruleset and its limits.

### macOS

Networking needs macOS 26 or later, and no root or extra entitlement.

**macOS 26 and later.** Each sandbox has a vmnet shared network of its own
(`VZVmnetNetworkDeviceAttachment`): vmnet allocates a separate /24 for each
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

**Limits on every macOS version.**

- A networked sandbox can connect to services on the Mac that listen on all
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
