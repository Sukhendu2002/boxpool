package agent

import (
	"os"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// GuestShutdown is the Shutdown of the agent in a VM. Reboot ends the VM:
// Firecracker exits on a guest reboot, with reboot=k on x86_64 and through
// PSCI on arm64.
func GuestShutdown() *Shutdown {
	return &Shutdown{
		Pids: procPids,
		Kill: syscall.Kill,
		Remount: func() error {
			unix.Sync()
			return unix.Mount("", "/", "", unix.MS_REMOUNT|unix.MS_RDONLY, "")
		},
		Reboot: func() error { return unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART) },
	}
}

// procPids lists the processes in /proc that have an executable: kernel
// threads and zombies have none.
func procPids() ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if _, err := os.Readlink("/proc/" + e.Name() + "/exe"); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}
