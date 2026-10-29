package agent

import (
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestProcPids(t *testing.T) {
	// A child that has exited but is not reaped yet: a zombie.
	zombie := exec.Command("true")
	if err := zombie.Start(); err != nil {
		t.Fatal(err)
	}
	defer zombie.Wait()
	stat := "/proc/" + strconv.Itoa(zombie.Process.Pid) + "/stat"
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if b, _ := os.ReadFile(stat); strings.Contains(string(b), ") Z ") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not become a zombie")
		}
	}
	pids, err := procPids()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(pids, os.Getpid()) {
		t.Error("procPids leaves out this process")
	}
	if slices.Contains(pids, zombie.Process.Pid) {
		t.Error("procPids lists a zombie")
	}
	// kthreadd, the parent of every kernel thread, is pid 2 when this
	// pid namespace shows it.
	if b, err := os.ReadFile("/proc/2/comm"); err == nil && strings.TrimSpace(string(b)) == "kthreadd" && slices.Contains(pids, 2) {
		t.Error("procPids lists kthreadd")
	}
}
