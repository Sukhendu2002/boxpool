package backend

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuestIdentity(t *testing.T) {
	dir := t.TempDir()
	script := configureGuest
	for _, path := range []string{"/proc/sys/kernel/hostname", "/etc/hostname", "/etc/machine-id", "/etc/hosts", "/var/lib/dbus/machine-id"} {
		replacement := filepath.Join(dir, strings.ReplaceAll(path, "/", "-"))
		script = strings.ReplaceAll(script, path, replacement)
		if err := os.WriteFile(replacement, []byte("old"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	name := "literal; not-a-command"
	machineID := "1234567890abcdef1234567890abcdef"
	run := func(fresh string) {
		t.Helper()
		cmd := exec.Command("sh", "-eu", "-c", script, "sh", name, machineID, fresh, "0", "", "", "", "", "", "")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("guest setup: %v: %s", err, out)
		}
	}
	run("1")
	for _, tc := range []struct{ file, want string }{
		{"-proc-sys-kernel-hostname", name + "\n"}, {"-etc-hostname", name + "\n"},
		{"-etc-machine-id", machineID + "\n"}, {"-var-lib-dbus-machine-id", machineID + "\n"},
	} {
		path := filepath.Join(dir, tc.file)
		if got, err := os.ReadFile(path); err != nil || string(got) != tc.want {
			t.Fatalf("%s: %q, %v", path, got, err)
		}
		if err := os.WriteFile(path, []byte("user identity"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	run("0")
	if got, _ := os.ReadFile(filepath.Join(dir, "-etc-machine-id")); string(got) != "user identity" {
		t.Fatal("restart overwrote existing identity")
	}
}
