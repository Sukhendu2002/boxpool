package image_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Exercise APT's actual transport with a private URL and output file. This
// never updates host package lists, installs packages, or needs root. Linux
// image CI has apt-helper; macOS runs the script/unit suite without it.
func TestAPTDownloads(t *testing.T) {
	const helper = "/usr/lib/apt/apt-helper"
	if _, err := os.Stat(helper); err != nil {
		if os.IsNotExist(err) {
			t.Skip("APT transport needs apt-helper")
		}
		t.Fatal(err)
	}
	config, err := filepath.Abs("apt.conf")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("verified package contents\n")
	sum := sha256.Sum256(payload)
	for _, mode := range []string{"transient", "mixed", "stalled", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			var attempts, slowAttempts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/slow" {
					slowAttempts.Add(1)
					w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
					for _, b := range payload {
						w.Write([]byte{b})
						w.(http.Flusher).Flush()
						select {
						case <-time.After(60 * time.Millisecond):
						case <-r.Context().Done():
							return
						}
					}
					return
				}
				n := attempts.Add(1)
				switch {
				case mode == "stalled":
					<-r.Context().Done()
				case (mode == "transient" || mode == "mixed") && n == 1:
					http.Error(w, "try again", http.StatusServiceUnavailable)
				case mode == "corrupt":
					w.Write([]byte("tampered package contents\n"))
				default:
					w.Write(payload)
				}
			}))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			dst := filepath.Join(t.TempDir(), "package")
			bin := t.TempDir()
			writeTestFile(t, filepath.Join(bin, "sleep"), "#!/bin/sh\nexit 0\n", 0755)
			// Replace only the apt-get executable. The checked-in wrapper and
			// GNU timeout run against APT's real transport and hash verifier.
			writeTestFile(t, filepath.Join(bin, "apt-get"), `#!/bin/sh
set -- /usr/lib/apt/apt-helper -c "$TEST_CONFIG" -o Acquire::http::Timeout=1 -o Acquire::http::Proxy::127.0.0.1=DIRECT download-file "$TEST_URL/package" "$TEST_DST" "$TEST_HASH"
if [ "$TEST_MODE" = mixed ]; then
    set -- "$@" "$TEST_URL/slow" "$TEST_DST.slow" "$TEST_HASH"
fi
exec "$@"
`, 0755)
			cmd := exec.CommandContext(ctx, "sh", "apt-retry.sh", "update")
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "TEST_CONFIG="+config,
				"TEST_URL="+srv.URL, "TEST_DST="+dst, "TEST_HASH=SHA256:"+hex.EncodeToString(sum[:]), "TEST_MODE="+mode)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.WaitDelay = time.Second
			cmd.Cancel = func() error {
				err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				if err == syscall.ESRCH {
					return os.ErrProcessDone
				}
				return err
			}
			start := time.Now()
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("APT failed to enforce its transfer timeout: %s", out)
			}
			if mode == "transient" || mode == "mixed" {
				b, readErr := os.ReadFile(dst)
				if err != nil || readErr != nil || string(b) != string(payload) || attempts.Load() < 2 {
					t.Fatalf("retry: %s, err %v, file %q (%v), requests %d", out, err, b, readErr, attempts.Load())
				}
			} else if err == nil {
				t.Fatalf("accepted %s download: %s", mode, out)
			}
			if strings.Contains(string(out), "Tried to start delayed item") {
				t.Fatalf("APT entered delayed retry queue: %s", out)
			}
			if mode == "mixed" {
				b, readErr := os.ReadFile(dst + ".slow")
				if readErr != nil || string(b) != string(payload) || slowAttempts.Load() == 0 {
					t.Fatalf("slow transfer: %q (%v)", b, readErr)
				}
			}
			t.Logf("%s: requests=%d slow=%d elapsed=%s", mode, attempts.Load(), slowAttempts.Load(), time.Since(start).Round(time.Millisecond))
		})
	}
}
