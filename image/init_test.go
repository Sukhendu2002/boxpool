package image_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise the real init's mount control flow without touching host mounts or
// /dev. Only /proc/mounts is redirected to a fixture; external commands are
// stubs. The actual kernel/mount implementation is covered by the KVM smoke.
func TestInitDevtmpfsContract(t *testing.T) {
	script, err := os.ReadFile("init")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, mounts, procExit, remountExit, want string
		code                                      int
	}{
		{name: "kernel automount", mounts: "devtmpfs /dev devtmpfs rw,relatime 0 0\n", code: 99, want: "reached-sysfs"},
		{name: "missing mount", code: 1, want: "kernel did not mount /dev"},
		{name: "wrong filesystem", mounts: "tmpfs /dev tmpfs rw 0 0\n", code: 1, want: "/dev must be devtmpfs"},
		{name: "proc failure", procExit: "31", code: 31},
		{name: "remount failure", mounts: "devtmpfs /dev devtmpfs rw 0 0\n", remountExit: "32", code: 32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write := func(name, content string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0700); err != nil {
					t.Fatal(err)
				}
			}
			write("mounts", tc.mounts)
			write("init", strings.ReplaceAll(string(script), "/proc/mounts", `"$INIT_MOUNTS"`))
			write("mount", `#!/bin/sh
case "$*" in
    *' /proc') exit "${PROC_EXIT:-0}" ;;
    *' /dev')
        # A fresh mount over the kernel's devtmpfs reproduces EBUSY. Require
        # remount and nosuid, then allow an injected real-mount failure.
        case ",$4," in *,remount,*) ;; *) exit 32 ;; esac
        case ",$4," in *,nosuid,*) ;; *) exit 33 ;; esac
        exit "${REMOUNT_EXIT:-0}" ;;
    *' /sys') echo reached-sysfs; exit 99 ;;
    *) exit 98 ;;
esac
`)
			// The sysfs sentinel stops init before cd/exec. Never mutate /dev.
			write("mkdir", "#!/bin/sh\nexit 0\n")
			write("ln", "#!/bin/sh\nexit 0\n")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", filepath.Join(dir, "init"))
			cmd.Env = append(os.Environ(), "PATH="+dir, "INIT_MOUNTS="+filepath.Join(dir, "mounts"),
				"PROC_EXIT="+tc.procExit, "REMOUNT_EXIT="+tc.remountExit)
			out, err := cmd.CombinedOutput()
			if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != tc.code || !strings.Contains(string(out), tc.want) {
				t.Fatalf("init: %s (%v), want exit %d containing %q", out, err, tc.code, tc.want)
			}
			if tc.code != 99 && strings.Contains(string(out), "reached-sysfs") {
				t.Fatalf("init continued after mount failure: %s", out)
			}
		})
	}
}
