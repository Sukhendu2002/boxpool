package preflight

import (
	"strings"
	"testing"
)

func TestMajorAtLeast(t *testing.T) {
	cases := []struct {
		v    string
		min  int
		want bool
	}{
		{"14.0", 14, true},
		{"15.3.1", 14, true},
		{"13.6", 14, false},
		{"26", 14, true},
		{"", 14, false},
		{"abc", 14, false},
	}
	for _, c := range cases {
		if got := MajorAtLeast(c.v, c.min); got != c.want {
			t.Errorf("MajorAtLeast(%q, %d) = %v, want %v", c.v, c.min, got, c.want)
		}
	}
}

func TestHasVirtFlag(t *testing.T) {
	if !hasVirtFlag("processor : 0\nflags : fpu vme vmx sse\n") {
		t.Error("expected vmx to be found")
	}
	if hasVirtFlag("flags : fpu sse\n") {
		t.Error("expected no virt flag")
	}
}

func TestKernelAtLeast510(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{"5.10.0-28-amd64", true},
		{"6.8.0", true},
		{"5.10", true},
		{"5.10.0-rc1", true},
		{" 6.8.0\n", true},
		{"5.4.0", false},
		{"4.19.0", false},
		{"5.9.99", false},
		{"garbage", false},
		{"", false},
		{"6", false},
		{"6.nope.0", false},
		{"6.-1.0", false},
	}
	for _, c := range cases {
		t.Run(c.version, func(t *testing.T) {
			if got := kernelAtLeast510(c.version); got != c.want {
				t.Errorf("kernelAtLeast510(%q) = %v, want %v", c.version, got, c.want)
			}
		})
	}
}

func TestKernelCheck(t *testing.T) {
	for _, version := range []string{"5.4.0", "garbage"} {
		c := kernelCheck(version)
		if c.OK || !c.Required || c.Warn || !strings.Contains(c.Detail, "kernel 5.10 or newer required") {
			t.Errorf("kernelCheck(%q) = %+v", version, c)
		}
	}
	c := kernelCheck("6.8.0\n")
	if !c.OK || !c.Required || c.Detail != "6.8.0" {
		t.Errorf("kernelCheck(6.8.0) = %+v", c)
	}
}

func TestIsVM(t *testing.T) {
	cases := []struct {
		name    string
		cpuinfo string
		vendor  string
		want    bool
	}{
		{"hypervisor flag", "processor : 0\nflags : fpu vmx hypervisor sse\n", "", true},
		{"later CPU", "flags : fpu\n\tflags\t: svm hypervisor\n", "", true},
		{"arm features", "Features : fp asimd hypervisor\n", "", true},
		{"physical CPU", "flags : fpu vmx sse\n", "Dell Inc.", false},
		{"flag substring", "flags : nohypervisor\n", "", false},
		{"model name", "model name : hypervisor\n", "", false},
		{"empty", "", "", false},
		{"QEMU", "", "QEMU\n", true},
		{"KVM", "", "KVM", true},
		{"VMware", "", "VMware, Inc.", true},
		{"Microsoft", "", "Microsoft Corporation", true},
		{"Hyper-V", "", "Hyper-V", true},
		{"Xen", "", "Xen", true},
		{"innotek", "", "innotek GmbH", true},
		{"VirtualBox", "", "VirtualBox", true},
		{"EC2", "", "Amazon EC2", true},
		{"Google", "", "Google", true},
		{"case insensitive", "", "qEmU", true},
		{"unknown vendor", "", "Acme Computers", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isVM(c.cpuinfo, c.vendor); got != c.want {
				t.Errorf("isVM(%q, %q) = %v, want %v", c.cpuinfo, c.vendor, got, c.want)
			}
		})
	}
}

func TestVMCheck(t *testing.T) {
	c := vmCheck("flags : hypervisor\n", "")
	if c.OK || !c.Warn || c.Required || !strings.Contains(c.Detail, "running inside a VM") {
		t.Errorf("VM should only warn: %+v", c)
	}
	c = vmCheck("flags : vmx\n", "Dell Inc.")
	if !c.OK || c.Warn || c.Required || c.Detail != "no VM indicators detected" {
		t.Errorf("unexpected physical host check: %+v", c)
	}
}

func TestReady(t *testing.T) {
	cases := []struct {
		name    string
		backend string
		checks  []Check
		want    bool
	}{
		{"no backend", "none", nil, false},
		{"backend available", "firecracker", nil, true},
		{"required passed", "firecracker", []Check{{OK: true, Required: true}}, true},
		{"disk failure", "firecracker", []Check{{Name: "free disk", Required: true}}, false},
		{"kernel failure", "firecracker", []Check{kernelCheck("5.4.0")}, false},
		{"docker still needs disk", "docker (weaker isolation)", []Check{{Name: "free disk", Required: true}}, false},
		{"docker still needs kernel", "docker (weaker isolation)", []Check{kernelCheck("5.4.0")}, false},
		{"optional failure", "docker (weaker isolation)", []Check{{Name: "/dev/kvm"}}, true},
		{"VM warning", "firecracker", []Check{vmCheck("flags : hypervisor", "")}, true},
		{"VM warning with docker", "docker (weaker isolation)", []Check{vmCheck("", "QEMU")}, true},
		{"warning without backend", "none", []Check{vmCheck("", "QEMU")}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Report{Backend: c.backend, Checks: c.checks}
			if got := r.Ready(); got != c.want {
				t.Errorf("Ready() = %v, want %v", got, c.want)
			}
			if r.Backend != c.backend {
				t.Errorf("Ready changed backend to %q", r.Backend)
			}
		})
	}
}

func TestPickBackend(t *testing.T) {
	for _, c := range []struct {
		force       string
		kvm, docker bool
		want        string
		check       bool // a BOXPOOL_BACKEND check is returned
		ok          bool // and it passes
	}{
		{"", true, true, "firecracker", false, false},
		{"", true, false, "firecracker", false, false},
		{"", false, true, "docker", false, false},
		{"", false, false, "none", false, false},
		{"docker", true, true, "docker", true, true},
		{"docker", true, false, "none", true, false},
		{"firecracker", false, true, "none", true, false},
		{"firecracker", true, false, "firecracker", true, true},
		{"gvisor", true, true, "none", true, false},
	} {
		got, check := pickBackend(c.force, c.kvm, c.docker)
		if got != c.want || (check != nil) != c.check || check != nil && (check.OK != c.ok || !check.Required) {
			t.Errorf("pickBackend(%q, kvm %v, docker %v) = %q, %+v; want %q", c.force, c.kvm, c.docker, got, check, c.want)
		}
		if r := (Report{Backend: got, Checks: []Check{{Required: true, OK: c.ok || check == nil}}}); r.Ready() != (c.want != "none") {
			t.Errorf("BOXPOOL_BACKEND=%q kvm %v docker %v: Ready %v", c.force, c.kvm, c.docker, r.Ready())
		}
	}
}
