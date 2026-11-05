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

func TestAPTOperationRetry(t *testing.T) {
	for _, mode := range []string{"update", "download", "timeout", "unavailable", "install-failure"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			writeTestFile(t, filepath.Join(dir, "timeout"), `#!/bin/sh
set -eu
test "$1" = --kill-after=5s
printf '%s\n' "$2" >> "$STATE/timeouts"
shift 2
if [ "$MODE" = timeout ] && [ ! -f "$STATE/timedout" ]; then
    test "$1" = apt-get
    shift
    printf '%s\n' "$*" >> "$STATE/calls"
    echo verified > "$STATE/archive"
    echo 1 > "$STATE/attempts"
    touch "$STATE/timedout"
    exit 124
fi
exec "$@"
`, 0755)
			writeTestFile(t, filepath.Join(dir, "sleep"), "#!/bin/sh\nprintf '%s\\n' \"$1\" >> \"$STATE/delays\"\n", 0755)
			writeTestFile(t, filepath.Join(dir, "apt-get"), `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$STATE/calls"
case "$*" in
    '-o APT::Update::Error-Mode=any update'|*--download-only*)
        n=0
        if [ -f "$STATE/attempts" ]; then n=$(cat "$STATE/attempts"); fi
        n=$((n+1)); echo "$n" > "$STATE/attempts"
        if [ "$MODE" = unavailable ]; then exit 42; fi
        if [ "$MODE" != install-failure ]; then
            if [ "$n" -eq 1 ]; then echo verified > "$STATE/archive"; exit 42; fi
            test "$(cat "$STATE/archive")" = verified
        fi
        ;;
    *--no-download*)
        echo installed >> "$STATE/installed"
        if [ "$MODE" = install-failure ]; then exit 43; fi
        ;;
    *) exit 99 ;;
esac
`, 0755)
			args := []string{"apt-retry.sh", "install", "example-package"}
			if mode == "update" {
				args = []string{"apt-retry.sh", "update"}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", args...)
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "STATE="+dir, "MODE="+mode)
			cmd.WaitDelay = time.Second
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("unbounded operation: %s", out)
			}
			wantExit := 0
			if mode == "unavailable" {
				wantExit = 42
			} else if mode == "install-failure" {
				wantExit = 43
			}
			gotExit := 0
			if err != nil {
				if e, ok := err.(*exec.ExitError); ok {
					gotExit = e.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			if gotExit != wantExit {
				t.Fatalf("exit %d, want %d: %s", gotExit, wantExit, out)
			}
			calls, err := os.ReadFile(filepath.Join(dir, "calls"))
			if err != nil {
				t.Fatal(err)
			}
			wantDownloads, wantInstalls := 2, 1
			if mode == "update" {
				wantInstalls = 0
			} else if mode == "unavailable" {
				wantDownloads, wantInstalls = 3, 0
			} else if mode == "install-failure" {
				wantDownloads = 1
			}
			lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
			if len(lines) != wantDownloads+wantInstalls || strings.Count(string(calls), "--no-download") != wantInstalls {
				t.Fatalf("unexpected operation sequence: %s", calls)
			}
			if wantInstalls == 1 && lines[len(lines)-1] != "install --no-download -y --no-install-recommends example-package" {
				t.Fatalf("install must run last, once, without downloads: %s", calls)
			}
			timeouts, err := os.ReadFile(filepath.Join(dir, "timeouts"))
			if err != nil {
				t.Fatal(err)
			}
			limit := "150s\n"
			if mode == "update" {
				limit = "60s\n"
			}
			if string(timeouts) != strings.Repeat(limit, wantDownloads) {
				t.Fatalf("attempt limits %q, want %d of %q", timeouts, wantDownloads, limit)
			}
			delays, _ := os.ReadFile(filepath.Join(dir, "delays"))
			wantDelays := "10"
			if mode == "unavailable" {
				wantDelays = "10\n20"
			} else if mode == "install-failure" {
				wantDelays = ""
			}
			if strings.TrimSpace(string(delays)) != wantDelays {
				t.Fatalf("retry delays %q, want %q", delays, wantDelays)
			}
		})
	}
}
