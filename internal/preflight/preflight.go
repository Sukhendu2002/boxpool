// Package preflight checks whether this machine can host Boxpool sandboxes.
package preflight

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// Check is the result of one preflight check.
type Check struct {
	Name   string
	OK     bool
	Detail string
}

// Report is the outcome of all checks plus the backend Boxpool would use.
type Report struct {
	OS      string
	Arch    string
	Backend string
	Checks  []Check
}

// Run performs the checks for the current OS.
func Run() Report {
	r := Report{OS: runtime.GOOS, Arch: runtime.GOARCH}
	switch runtime.GOOS {
	case "linux":
		r.Checks, r.Backend = linuxChecks()
	case "darwin":
		r.Checks, r.Backend = darwinChecks()
	default:
		r.Checks = []Check{{Name: "supported OS", OK: false, Detail: runtime.GOOS + " is not supported in v1"}}
		r.Backend = "none"
	}
	return r
}

func linuxChecks() ([]Check, string) {
	var checks []Check

	kvm := Check{Name: "/dev/kvm"}
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err == nil {
		f.Close()
		kvm.OK, kvm.Detail = true, "present and accessible"
	} else if os.IsNotExist(err) {
		kvm.Detail = "missing: enable virtualization in BIOS or load the kvm module"
	} else {
		kvm.Detail = "not accessible: add your user to the kvm group"
	}
	checks = append(checks, kvm)

	if runtime.GOARCH == "amd64" {
		virt := Check{Name: "CPU virtualization (vmx/svm)"}
		if b, err := os.ReadFile("/proc/cpuinfo"); err == nil && hasVirtFlag(string(b)) {
			virt.OK, virt.Detail = true, "supported"
		} else {
			virt.Detail = "not reported by the CPU"
		}
		checks = append(checks, virt)
	}

	docker := Check{Name: "docker (fallback)"}
	if _, err := exec.LookPath("docker"); err == nil {
		docker.OK, docker.Detail = true, "found"
	} else {
		docker.Detail = "not found"
	}
	checks = append(checks, docker)

	switch {
	case kvm.OK:
		return checks, "firecracker"
	case docker.OK:
		return checks, "docker (weaker isolation)"
	default:
		return checks, "none"
	}
}

func darwinChecks() ([]Check, string) {
	var checks []Check

	arch := Check{Name: "Apple silicon"}
	if runtime.GOARCH == "arm64" {
		arch.OK, arch.Detail = true, "arm64"
	} else {
		arch.Detail = "Intel Macs are not supported"
	}
	checks = append(checks, arch)

	ver := Check{Name: "macOS 14 or later"}
	out, err := exec.Command("sw_vers", "-productVersion").Output()
	if err != nil {
		ver.Detail = "could not read macOS version"
	} else {
		v := strings.TrimSpace(string(out))
		ver.OK = MajorAtLeast(v, 14)
		ver.Detail = v
	}
	checks = append(checks, ver)

	if arch.OK && ver.OK {
		return checks, "virtualization framework"
	}
	return checks, "none"
}

func hasVirtFlag(cpuinfo string) bool {
	for _, line := range strings.Split(cpuinfo, "\n") {
		if !strings.HasPrefix(line, "flags") {
			continue
		}
		for _, f := range strings.Fields(line) {
			if f == "vmx" || f == "svm" {
				return true
			}
		}
	}
	return false
}

// MajorAtLeast reports whether a version string like "14.2.1" has a major
// version of at least min.
func MajorAtLeast(version string, min int) bool {
	major, _, _ := strings.Cut(version, ".")
	n, err := strconv.Atoi(major)
	return err == nil && n >= min
}

// Ready reports whether a usable backend was found.
func (r Report) Ready() bool { return r.Backend != "none" }
