//go:build linux || darwin

package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"boxpool.run/boxpool/internal/daemon"
)

func TestClientSocketErrors(t *testing.T) {
	for _, problem := range []string{"missing", "refused", "permission", "permission-missing"} {
		t.Run(problem, func(t *testing.T) {
			permission := strings.HasPrefix(problem, "permission")
			if permission && os.Geteuid() == 0 {
				t.Skip("root bypasses directory permissions; client classification is tested separately")
			}
			dir, err := os.MkdirTemp("", "bp-cli") // Short Unix socket path for macOS.
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(dir) })
			sock := filepath.Join(dir, daemon.SocketName)
			if problem == "refused" || problem == "permission" {
				l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { l.Close() })
				if problem == "refused" {
					l.SetUnlinkOnClose(false)
					if err := l.Close(); err != nil {
						t.Fatal(err)
					}
				}
			}
			if permission {
				// Simulate a root-owned 0750 directory: lookup fails even without a socket.
				if err := os.Chmod(dir, 0000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Chmod(dir, 0700) })
			}
			for _, tt := range []struct {
				args []string
				code int
			}{
				{[]string{"sh"}, 1},
				{[]string{"exec", "box", "true"}, execFailed},
				{[]string{"cp", "box:/tmp/file", "./file"}, 1},
				{[]string{"ls"}, 1},
				{[]string{"stop", "box"}, 1},
				{[]string{"rm", "box"}, 1},
				{[]string{"version"}, 0},
			} {
				t.Run(tt.args[0], func(t *testing.T) {
					var stderr string
					if tt.args[0] == "sh" {
						r, _ := boxpool(t, dir, tt.args...)
						stderr = r.wait(t, tt.code)
					} else {
						code, stdout, errOut := cli(t, dir, "", tt.args...)
						stderr = errOut
						if code != tt.code {
							t.Errorf("exit = %d, want %d: %s", code, tt.code, stderr)
						}
						if tt.args[0] == "version" && !strings.HasPrefix(stdout, "boxpool ") {
							t.Errorf("local version missing: %q", stdout)
						}
					}
					if tt.args[0] == "version" && !permission {
						if stderr != "" {
							t.Errorf("offline version should stay quiet: %q", stderr)
						}
						return
					}
					if !strings.Contains(stderr, sock) {
						t.Errorf("socket missing from error: %q", stderr)
					}
					if permission {
						for _, want := range []string{"permission denied", "daemon status is unknown", "parent directory", "sudo boxpool <command>"} {
							if !strings.Contains(stderr, want) {
								t.Errorf("stderr = %q, want %q", stderr, want)
							}
						}
						if strings.Contains(stderr, "start it") || strings.Contains(stderr, "boxpool daemon") {
							t.Errorf("misleading start hint: %q", stderr)
						}
					} else if !strings.Contains(stderr, "sudo boxpool up") || !strings.Contains(stderr, "boxpool daemon") {
						t.Errorf("start hint missing: %q", stderr)
					}
				})
			}
			if problem == "permission-missing" {
				// Restore search access to model retrying as root or the daemon's user.
				if err := os.Chmod(dir, 0700); err != nil {
					t.Fatal(err)
				}
				code, _, stderr := cli(t, dir, "", "ls")
				if code != 1 || !strings.Contains(stderr, "sudo boxpool up") || strings.Contains(stderr, "permission denied") {
					t.Errorf("authorized probe = %d %q, want missing-socket startup hint", code, stderr)
				}
			}
		})
	}
}
