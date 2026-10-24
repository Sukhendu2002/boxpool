// Package vz is the backend for macOS on Apple silicon. Each sandbox is a
// Linux VM run by Apple's Virtualization framework, through the
// github.com/Code-Hex/vz binding (cgo), that boots the pinned arm64 kernel
// with VZLinuxBootLoader from the sandbox's own clone of the guest rootfs
// and has a virtio socket (vsock) device for the guest agent.
//
// A sandbox's files live in <state dir>/sandboxes/<id>/:
//
//	spec.json         the Spec it was created with
//	rootfs.ext4       its disk: an APFS clone (clonefile) of the base image
//	console.log       the guest's virtio console (hvc0)
//	console.log.1     its previous MiB or more, once it outgrew 1 MiB
//	resources.json    its persistent CPU, memory and disk reservation
//
// Create defaults to 2 vCPUs, 2048 MiB RAM and a 10 GiB disk, with the same
// admission as the Linux backends. The clone shares the base image's blocks
// until the guest writes, and Create extends it sparsely to Spec.DiskGB;
// every Start grows the guest's ext4 file system into it online with the
// EXT4_IOC_RESIZE_FS ioctl through the image's Perl, so the host needs no
// e2fsprogs (the pinned kernel prevents resize2fs opening a mounted disk).
//
// VMs run in the daemon's process (the framework's per-VM XPC helper ends
// with it), so none outlives the daemon: Close, on daemon shutdown, stops
// every running sandbox, and a new Driver sees all sandboxes as stopped.
// Stop freezes the guest's disk through the agent and powers the guest off
// with sysrq, falling back to the framework's stop request and then a
// forced stop (backend.ErrUncleanStop). Pause and Resume are not supported
// yet (#29).
//
// # Network
//
// Every VM has a virtio network device with a MAC address derived from its
// sandbox ID and gets outbound IPv4 through NAT, with no root needed. The
// guest configures eth0 by DHCP with the kernel's ip=dhcp, and every Start
// copies the nameserver the DHCP server gave the kernel, the NAT's DNS
// proxy, from /proc/net/pnp to /etc/resolv.conf.
//
// On macOS 26 and later each running VM has a vmnet shared network of its
// own (VZVmnetNetworkDeviceAttachment), on its own /24 of 192.168.64.0/19,
// and macOS forwards no packets between those networks, so sandboxes
// cannot reach each other. A process can hold at most 32 such networks, so
// at most 32 sandboxes run at once; a stopped VM's network is reused.
// Before macOS 26 (or when built with an older SDK) all VMs share the
// framework's NAT (VZNATNetworkDeviceAttachment) on one /24. It does not
// forward frames between VMs, but macOS routes between them, so root in one
// sandbox can add a route through the gateway and send packets to another
// (replies are lost). Either way a guest can reach the Mac's own services
// that listen on the gateway's or the Mac's other addresses. See
// docs/security.md.
//
// Creating a VM needs the com.apple.security.virtualization entitlement, so
// the daemon binary must be signed with it: scripts/dev-sign.sh ad-hoc signs
// a development build.
//
// The driver is built only on macOS; elsewhere this package holds just the
// platform-neutral VM configuration.
package vz
