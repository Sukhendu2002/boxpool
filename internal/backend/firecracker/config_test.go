package firecracker

import (
	"encoding/json"
	"testing"

	"boxpool.run/boxpool/internal/backend"
)

func TestSandboxLayout(t *testing.T) {
	l := sandboxLayout("/state/sandboxes", "abc")
	want := layout{
		dir:     "/state/sandboxes/abc",
		spec:    "/state/sandboxes/abc/spec.json",
		rootfs:  "/state/sandboxes/abc/rootfs.ext4",
		pid:     "/state/sandboxes/abc/firecracker.pid",
		api:     "/state/sandboxes/abc/fc.sock",
		vsock:   "/state/sandboxes/abc/vsock.sock",
		log:     "/state/sandboxes/abc/firecracker.log",
		console: "/state/sandboxes/abc/console.log",
	}
	if l != want {
		t.Errorf("got %+v, want %+v", l, want)
	}
}

func TestAPICalls(t *testing.T) {
	spec := backend.Spec{Name: "web", CPUs: 2, MemoryMB: 512, DiskGB: 10}
	calls := apiCalls(spec, "/a/vmlinux-amd64", sandboxLayout("/s", "abc"))
	want := []struct{ path, body string }{
		{"/machine-config", `{"mem_size_mib":512,"vcpu_count":2}`},
		{"/boot-source", `{"boot_args":"console=ttyS0 reboot=k panic=1 root=/dev/vda rw init=/sbin/init i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd","kernel_image_path":"/a/vmlinux-amd64"}`},
		{"/drives/rootfs", `{"drive_id":"rootfs","is_read_only":false,"is_root_device":true,"path_on_host":"/s/abc/rootfs.ext4"}`},
		{"/vsock", `{"guest_cid":3,"uds_path":"/s/abc/vsock.sock"}`},
		{"/actions", `{"action_type":"InstanceStart"}`},
	}
	if len(calls) != len(want) {
		t.Fatalf("got %d calls, want %d", len(calls), len(want))
	}
	for i, c := range calls {
		b, err := json.Marshal(c.body)
		if err != nil {
			t.Fatal(err)
		}
		if c.path != want[i].path || string(b) != want[i].body {
			t.Errorf("call %d: got PUT %s %s, want PUT %s %s", i, c.path, b, want[i].path, want[i].body)
		}
	}
}
