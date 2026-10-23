package vz

import (
	"fmt"
	"path/filepath"

	"boxpool.run/boxpool/internal/backend"
)

// bootArgs differ from Firecracker's: the Virtualization framework's console
// is a virtio console (hvc0), not a UART, and has no i8042 to skip. A guest
// reboot or panic (panic=1) stops the VM, as the framework cannot reboot one.
const bootArgs = "console=hvc0 panic=1 root=/dev/vda rw init=/sbin/init"

// agentPort is the vsock port boxpool-agent listens on.
const agentPort = 1024

// layout is the paths of one sandbox's files.
type layout struct {
	dir, spec, rootfs, console string
}

func sandboxLayout(root, id string) layout {
	dir := filepath.Join(root, id)
	f := func(name string) string { return filepath.Join(dir, name) }
	return layout{dir: dir, spec: f("spec.json"), rootfs: f("rootfs.ext4"), console: f("console.log")}
}

// limits are the Virtualization framework's bounds on a VM's size.
type limits struct {
	minCPUs, maxCPUs     uint
	minMemory, maxMemory uint64 // bytes
}

// vmConfig is the VM Start asks the Virtualization framework for. Every VM
// also gets a virtio entropy device and a virtio socket (vsock) device.
type vmConfig struct {
	CPUs        uint
	MemoryBytes uint64
	Kernel      string // an uncompressed arm64 Image, for VZLinuxBootLoader
	CommandLine string
	Disk        string // the sandbox's rootfs, a read-write virtio block device
	Console     string // the virtio console's output, appended to
}

// newVMConfig describes the VM for a sandbox created with spec. Create has
// already sized the disk to Spec.DiskGB GiB.
func newVMConfig(spec backend.Spec, kernel string, l layout, lim limits) (vmConfig, error) {
	spec, err := spec.WithDefaults()
	if err != nil {
		return vmConfig{}, err
	}
	c := vmConfig{
		CPUs:        uint(spec.CPUs),
		MemoryBytes: uint64(spec.MemoryMB) << 20,
		Kernel:      kernel,
		CommandLine: bootArgs,
		Disk:        l.rootfs,
		Console:     l.console,
	}
	if c.CPUs < lim.minCPUs || c.CPUs > lim.maxCPUs {
		return vmConfig{}, fmt.Errorf("%d CPUs is outside the Virtualization framework's %d to %d", c.CPUs, lim.minCPUs, lim.maxCPUs)
	}
	if c.MemoryBytes < lim.minMemory || c.MemoryBytes > lim.maxMemory {
		return vmConfig{}, fmt.Errorf("%d MiB of memory is outside the Virtualization framework's %d to %d MiB", spec.MemoryMB, lim.minMemory>>20, lim.maxMemory>>20)
	}
	return c, nil
}
