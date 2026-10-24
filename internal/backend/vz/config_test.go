package vz

import (
	"net"
	"path/filepath"
	"testing"

	"boxpool.run/boxpool/internal/backend"
)

var testLimits = limits{minCPUs: 1, maxCPUs: 8, minMemory: 256 << 20, maxMemory: 8 << 30}

func TestVMConfig(t *testing.T) {
	l := sandboxLayout("/state/sandboxes", "abc")
	if l.dir != filepath.Join("/state/sandboxes", "abc") || l.spec != filepath.Join(l.dir, "spec.json") {
		t.Fatalf("layout: %+v", l)
	}
	for _, spec := range []backend.Spec{{}, {CPUs: 1, MemoryMB: 512, DiskGB: 1}, {CPUs: 8, MemoryMB: 8192}} {
		got, err := newVMConfig(spec, "/artifacts/vmlinux-arm64", l, testLimits)
		if err != nil {
			t.Fatal(err)
		}
		defaults, _ := spec.WithDefaults()
		want := vmConfig{
			CPUs: uint(defaults.CPUs), MemoryBytes: uint64(defaults.MemoryMB) << 20,
			Kernel: "/artifacts/vmlinux-arm64", CommandLine: "console=hvc0 panic=1 root=/dev/vda rw init=/sbin/init ip=dhcp",
			Disk: filepath.Join(l.dir, "rootfs.ext4"), Console: filepath.Join(l.dir, "console.log"),
			MAC: macAddress("abc"),
		}
		if got != want {
			t.Errorf("Spec %+v: got %+v, want %+v", spec, got, want)
		}
	}
}

func TestVMConfigBounds(t *testing.T) {
	for _, spec := range []backend.Spec{{CPUs: -1}, {CPUs: 9}, {MemoryMB: 255}, {MemoryMB: 8193}, {DiskGB: -1}} {
		if _, err := newVMConfig(spec, "/kernel", layout{}, testLimits); err == nil {
			t.Errorf("accepted %+v", spec)
		}
	}
	lim := testLimits
	lim.minCPUs = 2
	if _, err := newVMConfig(backend.Spec{CPUs: 1}, "/kernel", layout{}, lim); err == nil {
		t.Error("accepted CPU count below framework minimum")
	}
}

func TestMACAddress(t *testing.T) {
	seen := map[string]bool{}
	for _, id := range []string{"a", "b", "abc", "0123456789ab"} {
		mac := macAddress(id)
		if mac != macAddress(id) {
			t.Errorf("%s: MAC is not stable", id)
		}
		hw, err := net.ParseMAC(mac)
		if err != nil || len(hw) != 6 {
			t.Fatalf("%s: %q: %v", id, mac, err)
		}
		if hw[0]&0x03 != 0x02 {
			t.Errorf("%s: %s is not a locally administered unicast address", id, mac)
		}
		if seen[mac] {
			t.Errorf("%s: %s is not unique", id, mac)
		}
		seen[mac] = true
	}
	if got := macAddress("abc"); got != "ba:78:16:bf:8f:01" {
		t.Errorf("macAddress(abc) = %s; changing it changes every sandbox's DHCP lease", got)
	}
}
