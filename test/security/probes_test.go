package securitytest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Execute the real script with a fixture filesystem. Redirect only its file
// lookups; keep shell conditionals and exit handling intact. These VM cases
// do not need privileged syscalls or a running hypervisor.
func TestNamespaceProbeGuestDevices(t *testing.T) {
	for _, tc := range []struct {
		name, backend, hostBoot, marker string
		wantPass                        bool
		wantOutput                      string
	}{
		{"vm guest memory device", "firecracker", "host-boot", "", true, "blocked:"},
		{"container host memory device", "docker", "host-boot", "", false, "host resource visible: /dev/mem"},
		{"vm shares host kernel", "firecracker", "guest-boot", "", false, ""},
		{"vm sees host files", "firecracker", "host-boot", "/host-private-marker", false, ""},
		{"vm sees host proc root", "firecracker", "host-boot", "/proc/1/root/host-private-marker", false, ""},
		{"unknown backend", "unknown", "host-boot", "", false, "unsupported backend"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := t.TempDir()
			files := map[string]string{"/dev/mem": "guest memory", "/proc/sys/kernel/random/boot_id": "guest-boot\n"}
			if tc.marker != "" {
				files[tc.marker] = "host secret"
			}
			for name, data := range files {
				path := filepath.Join(fixture, name)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			const fixtureLookups = `fixture=$1; shift
test() {
  local args=() arg
  for arg in "$@"; do
    case "$arg" in /*) arg="$fixture$arg";; esac
    args+=("$arg")
  done
  builtin test "${args[@]}"
}
cat() { command cat "$fixture$1"; }
# These are checked for availability but never invoked by the VM branch.
mount() { return 1; }
timeout() { return 1; }
perl() { return 1; }
`
			cmd := exec.Command("bash", "-c", fixtureLookups+namespaceProbe, "probe", fixture, tc.backend, tc.hostBoot, "/host-private-marker")
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.wantPass || !strings.Contains(string(out), tc.wantOutput) {
				t.Fatalf("probe exit: %v; output: %s", err, out)
			}
		})
	}
}
