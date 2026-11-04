package image_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Run the actual capture loop with injected X11 failures, without needing
// a display server or contacting Docker.
func TestDesktopCaptureRetry(t *testing.T) {
	source, err := os.ReadFile("test-desktop.sh")
	if err != nil {
		t.Fatal(err)
	}
	// Isolate the capture loop from the container and input-policy checks.
	a := strings.Index(string(source), "    screenshot=")
	b := strings.Index(string(source), "    test \"$(tigervncconfig")
	if a < 0 || b <= a {
		t.Fatal("capture loop not found")
	}
	loop := strings.ReplaceAll(string(source[a:b]), `'\''`, `'`)
	for _, mode := range []string{"transient", "empty", "persistent"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			stubs := map[string]string{
				"xdotool": "#!/bin/sh\nexit 0\n",
				"sleep":   "#!/bin/sh\nexit 0\n",
				"xwd": `#!/bin/sh
n=0
if [ -f "$STATE/attempts" ]; then n=$(cat "$STATE/attempts"); fi
n=$((n+1)); echo "$n" > "$STATE/attempts"
if [ "$MODE" = persistent ] || { [ "$MODE" = transient ] && [ "$n" -eq 1 ]; }; then
    echo 'X Error: BadWindow' >&2
    exit 1
fi
while [ "$1" != -out ]; do shift; done
if [ "$MODE" = empty ] && [ "$n" -eq 1 ]; then : > "$2"; else echo capture > "$2"; fi
`,
			}
			for name, script := range stubs {
				writeTestFile(t, filepath.Join(dir, name), script, 0755)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", "-ec", loop)
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "STATE="+dir, "MODE="+mode)
			cmd.WaitDelay = time.Second
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("capture retries did not terminate: %s", out)
			}
			attempts, readErr := os.ReadFile(filepath.Join(dir, "attempts"))
			if readErr != nil {
				t.Fatalf("capture did not run: %s (%v): %v", out, err, readErr)
			}
			want := 2
			if mode == "persistent" {
				want = 100
				if err == nil || !strings.Contains(string(out), "desktop did not produce") {
					t.Fatalf("persistent failure passed: %s (%v)", out, err)
				}
			} else if err != nil {
				t.Fatalf("capture did not recover: %s (%v)", out, err)
			}
			if strings.TrimSpace(string(attempts)) != strconv.Itoa(want) {
				t.Fatalf("capture attempts %s, want %d", attempts, want)
			}
		})
	}
}
