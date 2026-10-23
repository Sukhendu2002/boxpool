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
// yet (#29), and sandboxes have no network interface (#21).
//
// Creating a VM needs the com.apple.security.virtualization entitlement, so
// the daemon binary must be signed with it: scripts/dev-sign.sh ad-hoc signs
// a development build.
//
// The driver is built only on macOS; elsewhere this package holds just the
// platform-neutral VM configuration.
package vz
