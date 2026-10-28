// Package firecracker is the Firecracker backend for Linux with KVM. Each
// sandbox is one firecracker process, configured through its API socket,
// that boots the pinned kernel from the sandbox's own copy of the guest
// rootfs and has a vsock device for the guest agent.
//
// A sandbox's files live in <state dir>/sandboxes/<id>/:
//
//	spec.json         the Spec it was created with
//	rootfs.ext4       its disk: a reflink of the base image, or a sparse copy
//	firecracker.pid   the firecracker process, while it runs
//	fc.sock           Firecracker's API socket
//	vsock.sock        the host end of the guest's vsock device
//	firecracker.log   Firecracker's log
//	console.log       the guest's serial console
//	console.log.1     its previous MiB or more, once it outgrew 1 MiB
//	resources.json    its persistent CPU, memory and disk reservation
//	snapshot.vmstate  a paused sandbox's VM state
//	snapshot.mem      a paused sandbox's memory
//
// Create defaults to 2 vCPUs, 2048 MiB RAM and a 10 GiB disk. Shared
// admission keeps one CPU and max(1 GiB, 10% of RAM) for the host. Stopped
// sandboxes keep their reservations until Remove. The private sparse rootfs
// is grown with resize2fs (host e2fsprogs required); the cached base is never
// resized, and smaller-than-base requests are refused. Guest MemTotal is
// slightly below configured memory because the guest kernel reserves RAM.
//
// VMs run in a session of their own and outlive the daemon. New adopts
// each one whose agent answers, keeping its tap device, and kills the rest.
//
// Pause writes a full snapshot (VM state and all guest memory, so as much
// disk as the sandbox has RAM), syncs it and the rootfs to disk, ends the
// firecracker process and removes the tap. A paused sandbox has no process
// for New to adopt or kill, so it survives a daemon restart. Resume, or
// Start, recreates the tap, loads the snapshot, deletes it, resumes the
// vCPUs and sets the guest's clock, which stood still while paused. The
// deleted memory file keeps its disk space until the VM stops, because
// Firecracker maps it. Stop restores a paused sandbox first, to shut it
// down cleanly. Firecracker loads only snapshots from its own version, so a
// paused sandbox does not survive an upgrade of the pinned Firecracker.
//
// Firecracker runs without its jailer. The jailer needs root to chroot,
// create device nodes, join cgroups and drop to another uid, and boxpool's
// daemon and tests run unprivileged in v1. Firecracker's built-in seccomp
// filters stay on.
//
// # Network
//
// A Driver made with a store gives every running sandbox outbound IPv4 and
// nothing else; without one, sandboxes have no network interface. It needs
// root, ip and nft.
//
// Each sandbox has a slot in the store and the /30 at that slot in
// 10.200.0.0/16: the host's end of its tap device bp-<hash of the ID> is
// the gateway, the guest the next address, set with the kernel's ip= boot
// argument. Start creates the tap and Stop and Remove delete it; Remove
// frees the slot. Once the VM is gone, cleanup uses its own bounded
// context so caller cancellation does not strand the network. The guest
// /etc/resolv.conf is rewritten on every boot with the
// host's upstream nameservers that the host routes out of its outbound
// interface (from /run/systemd/resolve/resolv.conf when systemd-resolved
// runs, since its 127.0.0.53 stub is the host's), or else 1.1.1.1 and
// 8.8.8.8. So DNS never needs a host service.
//
// The nftables table inet boxpool (see ruleset) masquerades sandbox
// traffic out of the outbound interface, forwards only that and its
// replies, and drops packets from taps to the host itself (127.0.0.1-bound
// services are unreachable anyway; this covers its LAN, Docker and VPN
// addresses), to other taps and with a source address not routed through
// their tap. IPv6 from taps is dropped. Start enables IPv4 forwarding if it
// is off, and leaves it on. A drop in any table is final, but an accept is
// not: if an iptables FORWARD chain has policy DROP, as Docker and ufw set
// it, two accepts tagged boxpool for bp-* go into DOCKER-USER (which Docker
// leaves to users) or else at the top of FORWARD. The table and those
// accepts are installed before a tap is created and removed with the last
// tap, so a tap is never forwarded without the drops.
//
// Limits: the outbound interface is looked up at each Start, so a sandbox
// keeps the old one if the host's default route moves; other firewalls that
// drop forwarded traffic (firewalld, a final DROP rule under an ACCEPT
// policy) are not handled; two daemons on one host would race over the
// table. New removes orphaned TAPs only when their exact generated names,
// TAP type and ownership aliases match persisted allocations. An unrelated
// bp-* device is preserved, including at a sandbox's generated name (Start
// rejects that collision). Unmarked TAPs from an older binary or a lost
// allocation database need operator inspection; a prefix alone is not proof
// of ownership. The prefix remains reserved for the firewall's matching.
// TAPs become persistent only after their alias is installed, so a crash
// during creation cannot leave an unmarked persistent device. Keep the
// allocation database with the state directory for crash recovery.
//
// The driver is built only on Linux; elsewhere this package is empty.
package firecracker
