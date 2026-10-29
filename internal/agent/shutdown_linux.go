package agent

import (
	"bytes"
	"os"
	"strconv"
	"strings"
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

// pfKthread is PF_KTHREAD in /proc/<pid>/stat's flags.
const pfKthread = 0x00200000

// procPids lists the processes in /proc that have a live thread, leaving
// out kernel threads and zombies. A leader that exited with pthread_exit
// is a zombie whose other threads still run, so it counts as live.
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
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue // it has exited
		}
		// After the ")" that ends the command name: state, ppid, pgrp,
		// session, tty_nr, tpgid, flags.
		f := strings.Fields(string(b[bytes.LastIndexByte(b, ')')+1:]))
		if len(f) < 7 {
			continue
		}
		if flags, _ := strconv.ParseUint(f[6], 10, 64); flags&pfKthread != 0 {
			continue
		}
		if f[0] == "Z" || f[0] == "X" {
			if tasks, err := os.ReadDir("/proc/" + e.Name() + "/task"); err != nil || len(tasks) <= 1 {
				continue
			}
		}
		pids = append(pids, pid)
	}
	return pids, nil
}
