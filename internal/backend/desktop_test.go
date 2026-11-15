package backend_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Exercise the actual guest scripts on either host OS, without KVM, VZ,
// Docker or X11. Parse the platform-tagged files rather than copying the loop.
func TestDesktopStartupCapture(t *testing.T) {
	for _, source := range []string{
		"firecracker/desktop_integration_linux_test.go",
		"docker/integration_linux_test.go",
		"vz/desktop_integration_darwin_test.go",
	} {
		t.Run(source, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), source, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			var scripts []string
			ast.Inspect(file, func(node ast.Node) bool {
				literal, ok := node.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return true
				}
				value, err := strconv.Unquote(literal.Value)
				if err == nil && strings.Contains(value, "xdotool search") && strings.Contains(value, "chromium --version") {
					scripts = append(scripts, value)
				}
				return true
			})
			if len(scripts) != 1 {
				t.Fatalf("found %d desktop guest scripts, want one", len(scripts))
			}
			for _, mode := range []string{"success", "window-race", "capture-race", "empty", "persistent", "no-window", "input-policy"} {
				t.Run(mode, func(t *testing.T) {
					dir := t.TempDir()
					stubs := map[string]string{
						"xdotool": `#!/bin/sh
n=0
if [ -f "$STATE/windows" ]; then n=$(cat "$STATE/windows"); fi
n=$((n+1)); echo "$n" > "$STATE/windows"
if [ "$MODE" = no-window ] || { [ "$MODE" = window-race ] && [ "$n" -eq 1 ]; }; then exit 1; fi
echo 4194306
`,
						"xwd": `#!/bin/sh
n=0
if [ -f "$STATE/captures" ]; then n=$(cat "$STATE/captures"); fi
n=$((n+1)); echo "$n" > "$STATE/captures"
out=/dev/stdout
while [ "$#" -gt 0 ]; do
    if [ "$1" = -out ]; then out=$2; shift; fi
    shift
done
if [ "$MODE" = persistent ] || { [ "$MODE" = capture-race ] && [ "$n" -eq 1 ]; }; then
    echo 'X Error: BadWindow' >&2
    exit 1
fi
if [ "$MODE" = empty ] && [ "$n" -eq 1 ]; then : > "$out"; else echo screenshot > "$out"; fi
`,
						"sleep":          "#!/bin/sh\nexit 0\n",
						"tigervncconfig": "#!/bin/sh\nif [ \"$MODE\" = input-policy ]; then echo 1; else echo 0; fi\n",
						"chromium":       "#!/bin/sh\necho Chromium-fixture\n",
					}
					for name, script := range stubs {
						if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0755); err != nil {
							t.Fatal(err)
						}
					}
					// Keep even the old one-shot capture inside this test directory.
					script := strings.ReplaceAll(scripts[0], "/tmp/screen.xwd", filepath.Join(dir, "screen.xwd"))
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					defer cancel()
					cmd := exec.CommandContext(ctx, "sh", "-ec", script)
					cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "STATE="+dir, "TMPDIR="+dir, "MODE="+mode)
					cmd.WaitDelay = time.Second
					out, err := cmd.CombinedOutput()
					if ctx.Err() != nil {
						t.Fatalf("capture loop did not terminate: %s", out)
					}
					wantFailure := mode == "persistent" || mode == "no-window" || mode == "input-policy"
					if wantFailure != (err != nil) {
						t.Fatalf("success=%v, want success=%v: %s", err == nil, !wantFailure, out)
					}
					wantWindows, wantCaptures := 1, 1
					switch mode {
					case "window-race":
						wantWindows = 2
					case "capture-race", "empty":
						wantWindows, wantCaptures = 2, 2
					case "persistent":
						wantWindows, wantCaptures = 100, 100
					case "no-window":
						wantWindows, wantCaptures = 100, 0
					}
					for name, want := range map[string]int{"windows": wantWindows, "captures": wantCaptures} {
						data, readErr := os.ReadFile(filepath.Join(dir, name))
						if readErr != nil && want != 0 {
							t.Fatal(readErr)
						}
						if got := strings.TrimSpace(string(data)); got != strconv.Itoa(want) && !(want == 0 && os.IsNotExist(readErr)) {
							t.Errorf("%s attempts %q, want %d", name, got, want)
						}
					}
					captures, err := filepath.Glob(filepath.Join(dir, "boxpool-desktop.*"))
					if err != nil || len(captures) != 0 {
						t.Errorf("capture files remain: %v (%v)", captures, err)
					}
					if !wantFailure && !strings.Contains(string(out), "Chromium-fixture") {
						t.Fatalf("script did not finish policy/version checks: %s", out)
					}
				})
			}
		})
	}
}
