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
	for _, mode := range []string{"transient", "stalled", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			var attempts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := attempts.Add(1)
				switch {
				case mode == "stalled":
					<-r.Context().Done()
				case mode == "transient" && n == 1:
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
			// Shorten only the transfer wait; retain the checked-in retry and
			// pipeline policy. Production uses a 30-second transfer timeout.
			cmd := exec.CommandContext(ctx, helper, "-c", config,
				"-o", "Acquire::http::Timeout=1", "-o", "Acquire::http::Proxy::127.0.0.1=DIRECT", "download-file",
				srv.URL+"/package", dst, "SHA256:"+hex.EncodeToString(sum[:]))
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
			if mode == "transient" {
				b, readErr := os.ReadFile(dst)
				if err != nil || readErr != nil || string(b) != string(payload) || attempts.Load() < 2 {
					t.Fatalf("retry: %s, err %v, file %q (%v), requests %d", out, err, b, readErr, attempts.Load())
				}
			} else if err == nil {
				t.Fatalf("accepted %s download: %s", mode, out)
			}
			t.Logf("%s: requests=%d elapsed=%s", mode, attempts.Load(), time.Since(start).Round(time.Millisecond))
		})
	}
}
