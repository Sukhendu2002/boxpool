// Package firecracker is the Firecracker backend for Linux with KVM. Each
// sandbox is one firecracker process, configured through its API socket,
// that boots the pinned kernel from the sandbox's own copy of the guest
// rootfs and has a vsock device for the guest agent. There is no network
// interface yet.
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
//
// Firecracker runs without its jailer. The jailer needs root to chroot,
// create device nodes, join cgroups and drop to another uid, and boxpool's
// daemon and tests run unprivileged in v1. Firecracker's built-in seccomp
// filters stay on.
//
// The driver is built only on Linux; elsewhere this package is empty.
package firecracker
