// Package resources admits sandbox allocations without overcommitting the
// node. All backends share the same state directory and allocation lock.
package resources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"boxpool.run/boxpool/internal/backend"
)

// Capacity is a snapshot of the node. CPUs lists the allowed logical CPU
// IDs, MemoryMB is total physical memory, AvailableMB excludes memory in
// active use, and DiskBytes is currently available space on the state FS.
type Capacity struct {
	CPUs                             []int
	MemoryMB, AvailableMB, DiskBytes int64
}

// Allocation survives Stop and daemon restarts. CPU IDs restrict Docker's
// affinity; Firecracker reserves a count but schedules its own vCPU threads.
type Allocation struct {
	Spec      backend.Spec
	CPUs      []int
	DiskBytes int64 // zero for Docker: plain named volumes have no quota
}

// Manager coordinates allocations for one node state directory. Probe may
// be supplied for deterministic capacity tests; nil reads the actual host.
type Manager struct {
	StateDir string
	Probe    func(string) (Capacity, error)
}

const manifest = "resources.json"

// directory is where kind keeps its sandboxes. The VM backends share
// sandboxes/, so allocations reads it once, as "firecracker".
func directory(kind string) string {
	if kind == "firecracker" || kind == "vz" {
		return "sandboxes"
	}
	return "docker"
}

// Create checks capacity and creates the private sandbox directory with a
// durable reservation before a backend allocates any resources. The caller
// removes the directory only after all resources have been removed. A crash
// or failed cleanup therefore leaves a conservative reservation for Remove.
func (m *Manager) Create(ctx context.Context, kind, id string, spec backend.Spec, diskBytes int64) (Allocation, error) {
	var result Allocation
	err := m.locked(ctx, func() error {
		dir := filepath.Join(m.StateDir, directory(kind), id)
		if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
			if err == nil {
				return fmt.Errorf("sandbox %s already exists", id)
			}
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
			return err
		}
		existing, err := m.allocations()
		if err != nil {
			return err
		}
		probe := m.Probe
		if probe == nil {
			probe = hostCapacity
		}
		// A relocated backend directory can be on a different filesystem
		// from the state database; its own filesystem backs the VM disks.
		capacity, err := probe(filepath.Dir(dir))
		if err != nil {
			return fmt.Errorf("read host capacity: %w", err)
		}
		result, err = Admit(capacity, existing, spec, diskBytes)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.Mkdir(dir, 0700); err != nil {
			return err
		}
		if err := writeAllocation(dir, result); err != nil {
			return errors.Join(err, os.RemoveAll(dir))
		}
		return nil
	})
	return result, err
}

// Adopt records a verified existing sandbox without applying admission to
// resources it already owns. Existing manifests are never overwritten.
func (m *Manager) Adopt(ctx context.Context, kind, id string, a Allocation) error {
	return m.locked(ctx, func() error {
		dir := filepath.Join(m.StateDir, directory(kind), id)
		if _, err := os.Lstat(filepath.Join(dir, manifest)); !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		return writeAllocation(dir, a)
	})
}

func writeAllocation(dir string, a Allocation) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".resources-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, manifest))
}

func (m *Manager) allocations() ([]Allocation, error) {
	var allocations []Allocation
	for _, kind := range []string{"firecracker", "docker"} {
		root := filepath.Join(m.StateDir, directory(kind))
		entries, err := os.ReadDir(root)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(root, e.Name())
			b, err := os.ReadFile(filepath.Join(dir, manifest))
			var a Allocation
			if errors.Is(err, fs.ErrNotExist) {
				// Firecracker predates manifests; its spec and real disk
				// size are enough to account for an existing VM.
				if kind != "firecracker" {
					return nil, fmt.Errorf("Docker allocation %s lacks resources.json: initialize the Docker backend to import existing limits, or Remove the sandbox", dir)
				}
				b, err = os.ReadFile(filepath.Join(dir, "spec.json"))
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				if err == nil {
					err = json.Unmarshal(b, &a.Spec)
				}
				if err == nil {
					var fi os.FileInfo
					fi, err = os.Stat(filepath.Join(dir, "rootfs.ext4"))
					if err == nil {
						a.DiskBytes = fi.Size()
					}
				}
			} else if err == nil {
				err = json.Unmarshal(b, &a)
			}
			if err == nil {
				a.Spec, err = a.Spec.WithDefaults()
			}
			if err != nil {
				return nil, fmt.Errorf("read allocation %s: %w", dir, err)
			}
			if a.DiskBytes < 0 {
				return nil, fmt.Errorf("invalid disk reservation in %s", dir)
			}
			allocations = append(allocations, a)
		}
	}
	return allocations, nil
}

// Admit keeps one CPU and max(1 GiB, 10% of total RAM) for the host. Memory
// is bounded by currently available RAM minus all existing commitments.
// This is conservative for running sandboxes, whose resident memory already
// reduces MemAvailable, and protects the future use of stopped/idle ones.
// All created sandboxes, including stopped ones, retain their commitments.
// Disk admission conservatively reserves each VM's entire size against
// current free space, including sparse holes and possible reflink CoW.
func Admit(c Capacity, existing []Allocation, spec backend.Spec, diskBytes int64) (Allocation, error) {
	var err error
	spec, err = spec.WithDefaults()
	if err != nil {
		return Allocation{}, err
	}
	cpuFree, memFree, diskFree, reserve := free(c, existing)
	used := map[int]bool{}
	for _, a := range existing {
		for _, cpu := range a.CPUs {
			used[cpu] = true
		}
	}
	if spec.CPUs > cpuFree {
		return Allocation{}, fmt.Errorf("insufficient CPU: requested %d, available %d after existing sandboxes and 1 CPU host reserve", spec.CPUs, cpuFree)
	}
	if int64(spec.MemoryMB) > memFree {
		return Allocation{}, fmt.Errorf("insufficient memory: requested %d MiB, available %d MiB after existing sandboxes and %d MiB host reserve", spec.MemoryMB, memFree, reserve)
	}
	if diskBytes < 0 || diskBytes > diskFree {
		return Allocation{}, fmt.Errorf("insufficient disk: requested %d bytes, available %d bytes after existing disk reservations", diskBytes, diskFree)
	}
	// Keep the first allowed CPU outside Docker's cpusets. Legacy
	// reservations without CPU IDs still reduce cpuFree above.
	var cpus []int
	for _, cpu := range c.CPUs[1:] {
		if !used[cpu] {
			cpus = append(cpus, cpu)
		}
		if len(cpus) == spec.CPUs {
			break
		}
	}
	if len(cpus) < spec.CPUs {
		return Allocation{}, fmt.Errorf("insufficient CPU affinity capacity: requested %d, available %d", spec.CPUs, len(cpus))
	}
	return Allocation{Spec: spec, CPUs: cpus, DiskBytes: diskBytes}, nil
}

// free is what Admit can give a new sandbox, after the host reserve and
// existing commitments.
func free(c Capacity, existing []Allocation) (cpus int, memMB, diskBytes, reserveMB int64) {
	reserveMB = max(int64(1024), (c.MemoryMB+9)/10)
	cpus = max(0, len(c.CPUs)-1)
	memMB = max(int64(0), min(c.MemoryMB, c.AvailableMB)-reserveMB)
	diskBytes = max(int64(0), c.DiskBytes)
	for _, a := range existing {
		cpus = max(0, cpus-a.Spec.CPUs)
		memMB = max(int64(0), memMB-int64(a.Spec.MemoryMB))
		diskBytes = max(int64(0), diskBytes-a.DiskBytes)
	}
	return cpus, memMB, diskBytes, reserveMB
}

// Free returns the CPUs and MiB of memory Create could admit now, as the
// scheduler compares nodes.
func (m *Manager) Free(ctx context.Context) (cpus int, memoryMB int64, err error) {
	r, err := m.Summary(ctx)
	return r.CPUs, r.MemoryMB, err
}

// Summary is admission capacity: totals exclude the host reserve; free
// capacity also excludes commitments and memory in active use.
type Summary struct {
	CPUs, TotalCPUs         int
	MemoryMB, TotalMemoryMB int64
}

func (m *Manager) Summary(ctx context.Context) (r Summary, err error) {
	err = m.locked(ctx, func() error {
		existing, err := m.allocations()
		if err != nil {
			return err
		}
		probe := m.Probe
		if probe == nil {
			probe = hostCapacity
		}
		c, err := probe(m.StateDir)
		if err != nil {
			return fmt.Errorf("read host capacity: %w", err)
		}
		r.CPUs, r.MemoryMB, _, _ = free(c, existing)
		r.TotalCPUs = max(0, len(c.CPUs)-1)
		r.TotalMemoryMB = max(0, c.MemoryMB-max(int64(1024), (c.MemoryMB+9)/10))
		return nil
	})
	return r, err
}

// CPUSet is the Docker Engine's comma-separated list of logical CPU IDs.
func (a Allocation) CPUSet() string {
	parts := make([]string, len(a.CPUs))
	for i, cpu := range a.CPUs {
		parts[i] = strconv.Itoa(cpu)
	}
	return strings.Join(parts, ",")
}

// ParseCPUSet accepts Docker's list/range syntax for adopted containers.
func ParseCPUSet(value string) ([]int, error) {
	var cpus []int
	if value == "" {
		return nil, nil
	}
	for _, part := range strings.Split(value, ",") {
		lo, hi, rangeOK := strings.Cut(part, "-")
		first, err := strconv.Atoi(lo)
		if err != nil || first < 0 {
			return nil, fmt.Errorf("invalid CPU set %q", value)
		}
		last := first
		if rangeOK {
			last, err = strconv.Atoi(hi)
		}
		if err != nil || last < first || last > 1<<20 {
			return nil, fmt.Errorf("invalid CPU set %q", value)
		}
		for cpu := first; cpu <= last; cpu++ {
			cpus = append(cpus, cpu)
		}
	}
	slices.Sort(cpus)
	return slices.Compact(cpus), nil
}
