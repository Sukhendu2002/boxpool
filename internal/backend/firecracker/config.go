package firecracker

import (
	"path/filepath"

	"boxpool.run/boxpool/internal/backend"
)

// bootArgs are the image smoke test's. reboot=k and panic=1 turn a guest
// reboot or panic into the firecracker process exiting; the i8042 options
// skip a keyboard probe that costs about 0.5 s (docs/benchmarks/m1-boot.md).
const bootArgs = "console=ttyS0 reboot=k panic=1 root=/dev/vda rw init=/sbin/init i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd"

const (
	// guestCID is the guest's vsock context ID. Every VM has its own vsock
	// device, so all guests can use 3, the first unreserved ID.
	guestCID = 3
	// agentPort is the vsock port boxpool-agent listens on.
	agentPort = 1024
)

// layout is the paths of one sandbox's files.
type layout struct {
	dir, spec, rootfs, pid, api, vsock, log, console string
}

func sandboxLayout(root, id string) layout {
	dir := filepath.Join(root, id)
	f := func(name string) string { return filepath.Join(dir, name) }
	return layout{
		dir:     dir,
		spec:    f("spec.json"),
		rootfs:  f("rootfs.ext4"),
		pid:     f("firecracker.pid"),
		api:     f("fc.sock"),
		vsock:   f("vsock.sock"),
		log:     f("firecracker.log"),
		console: f("console.log"),
	}
}

type apiCall struct {
	path string
	body any
}

// apiCalls returns the API requests, in order, that configure and boot a
// sandbox, with a network interface unless n is nil. Spec.DiskGB is not
// applied: the disk is the image's size until #13 adds limits.
func apiCalls(spec backend.Spec, kernel string, l layout, n *nic) []apiCall {
	args := bootArgs
	if n != nil {
		args += " " + n.bootArg()
	}
	calls := []apiCall{
		{"/machine-config", map[string]any{"vcpu_count": spec.CPUs, "mem_size_mib": spec.MemoryMB}},
		{"/boot-source", map[string]any{"kernel_image_path": kernel, "boot_args": args}},
		{"/drives/rootfs", map[string]any{"drive_id": "rootfs", "path_on_host": l.rootfs, "is_root_device": true, "is_read_only": false}},
		{"/vsock", map[string]any{"guest_cid": guestCID, "uds_path": l.vsock}},
	}
	if n != nil {
		calls = append(calls, apiCall{"/network-interfaces/eth0", map[string]any{"iface_id": "eth0", "host_dev_name": n.tap}})
	}
	return append(calls, apiCall{"/actions", map[string]any{"action_type": "InstanceStart"}})
}
