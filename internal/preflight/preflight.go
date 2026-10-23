// Package preflight checks whether this machine can host Boxpool sandboxes.
package preflight

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"boxpool.run/boxpool/internal/backend/docker"
)

// Check is the result of one preflight check.
type Check struct {
	Name     string
	OK       bool
	Warn     bool
	Required bool // Required checks must pass even when a fallback backend exists.
	Detail   string
}

// Report is the outcome of all checks plus the backend Boxpool would use.
// Warning is set when that backend is a fallback with weaker isolation;
// everything that picks it or creates sandboxes on it must show it.
type Report struct {
	OS      string
	Arch    string
	Backend string
	Warning string
	Checks  []Check
}

// StateDir returns the directory Boxpool keeps its state in: BOXPOOL_HOME
// when set, otherwise /var/lib/boxpool on Linux and
// ~/Library/Application Support/boxpool on macOS.
func StateDir() (string, error) {
	if dir := os.Getenv("BOXPOOL_HOME"); dir != "" {
		return dir, nil
	}
	if runtime.GOOS == "darwin" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", "boxpool"), nil
	}
	return "/var/lib/boxpool", nil
}

// Run performs the checks for the current OS.
func Run() Report {
	r := Report{OS: runtime.GOOS, Arch: runtime.GOARCH}
	switch runtime.GOOS {
	case "linux":
		r.Checks, r.Backend = linuxChecks()
		r.Checks = append(r.Checks, diskCheck())
		if r.Backend == "docker" {
			r.Warning = docker.Warning
		}
	case "darwin":
		r.Checks, r.Backend = darwinChecks()
		r.Checks = append(r.Checks, diskCheck())
	default:
		r.Checks = []Check{{Name: "supported OS", OK: false, Detail: runtime.GOOS + " is not supported in v1"}}
		r.Backend = "none"
	}
	return r
}

func linuxChecks() ([]Check, string) {
	var checks []Check
	kernel := Check{Name: "Linux kernel", Required: true}
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		kernel = kernelCheck(string(b))
	} else {
		kernel.Detail = "could not read kernel version: " + err.Error()
	}
	checks = append(checks, kernel)

	cpuinfo, cpuErr := os.ReadFile("/proc/cpuinfo")
	vendor, vendorErr := os.ReadFile("/sys/class/dmi/id/sys_vendor")
	vm := vmCheck(string(cpuinfo), string(vendor))
	if cpuErr != nil && vendorErr != nil {
		vm.OK, vm.Warn, vm.Detail = false, true, "could not determine whether running inside a VM"
	}
	checks = append(checks, vm)

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
		if cpuErr == nil && hasVirtFlag(string(cpuinfo)) {
			virt.OK, virt.Detail = true, "supported"
		} else {
			virt.Detail = "not reported by the CPU"
		}
		checks = append(checks, virt)
	}

	dockerCheck := Check{Name: "docker engine (fallback)"}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if v, err := docker.Probe(ctx); err == nil {
		dockerCheck.OK, dockerCheck.Detail = true, v
	} else {
		dockerCheck.Detail = err.Error()
	}
	checks = append(checks, dockerCheck)

	backend, force := pickBackend(os.Getenv("BOXPOOL_BACKEND"), kvm.OK, dockerCheck.OK)
	if force != nil {
		checks = append(checks, *force)
	}
	return checks, backend
}

// pickBackend returns the backend for a machine with or without usable KVM
// and Docker: Firecracker when KVM works, else Docker. BOXPOOL_BACKEND
// (force), if set, names the backend to use instead, and the returned check
// says whether it can.
func pickBackend(force string, kvm, docker bool) (string, *Check) {
	switch {
	case force == "":
	case force == "firecracker" && kvm, force == "docker" && docker:
		return force, &Check{Name: "BOXPOOL_BACKEND", OK: true, Required: true, Detail: "forced to " + force}
	case force == "firecracker" || force == "docker":
		return "none", &Check{Name: "BOXPOOL_BACKEND", Required: true, Detail: force + " is forced but not usable here"}
	default:
		return "none", &Check{Name: "BOXPOOL_BACKEND", Required: true, Detail: fmt.Sprintf("unknown backend %q: want firecracker or docker", force)}
	}
	switch {
	case kvm:
		return "firecracker", nil
	case docker:
		return "docker", nil
	}
	return "none", nil
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

func kernelAtLeast510(version string) bool {
	parts := strings.SplitN(strings.TrimSpace(version), ".", 3)
	if len(parts) < 2 {
		return false
	}
	major, majorErr := strconv.Atoi(parts[0])
	minor, minorErr := strconv.Atoi(parts[1])
	return majorErr == nil && minorErr == nil && minor >= 0 && (major > 5 || major == 5 && minor >= 10)
}

func kernelCheck(version string) Check {
	version = strings.TrimSpace(version)
	c := Check{Name: "Linux kernel", OK: kernelAtLeast510(version), Required: true, Detail: version}
	if !c.OK {
		c.Detail = fmt.Sprintf("%q: kernel 5.10 or newer required", version)
	}
	return c
}

func isVM(cpuinfo, vendor string) bool {
	for _, line := range strings.Split(cpuinfo, "\n") {
		key, value, _ := strings.Cut(line, ":")
		if strings.TrimSpace(key) != "flags" && strings.TrimSpace(key) != "Features" {
			continue
		}
		for _, flag := range strings.Fields(value) {
			if flag == "hypervisor" {
				return true
			}
		}
	}
	vendor = strings.ToLower(vendor)
	for _, name := range []string{"qemu", "kvm", "vmware", "microsoft corporation", "hyper-v", "xen", "innotek", "virtualbox", "amazon ec2", "google"} {
		if strings.Contains(vendor, name) {
			return true
		}
	}
	return false
}

func vmCheck(cpuinfo, vendor string) Check {
	c := Check{Name: "VM host", OK: true, Detail: "no VM indicators detected"}
	if isVM(cpuinfo, vendor) {
		c.OK, c.Warn, c.Detail = false, true, "running inside a VM: nested virtualization may be unavailable or slow"
	}
	return c
}

// MajorAtLeast reports whether a version string like "14.2.1" has a major
// version of at least min.
func MajorAtLeast(version string, min int) bool {
	major, _, _ := strings.Cut(version, ".")
	n, err := strconv.Atoi(major)
	return err == nil && n >= min
}

// Ready reports whether a usable backend was found and its prerequisites pass.
func (r Report) Ready() bool {
	if r.Backend == "none" {
		return false
	}
	for _, c := range r.Checks {
		if c.Required && !c.OK && !c.Warn {
			return false
		}
	}
	return true
}
