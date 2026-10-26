//go:build darwin && cgo && integration

package vz

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"sort"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/agent"
	"boxpool.run/boxpool/internal/artifacts"
	"boxpool.run/boxpool/internal/backend"
)

// TestImageBoot compares locally built images without changing release pins.
// It measures a new VM and disk each time, with host caches left warm.
func TestImageBoot(t *testing.T) {
	home, kernel, rootfs := os.Getenv("BENCH_HOME"), os.Getenv("BENCH_KERNEL"), os.Getenv("BENCH_ROOTFS")
	if home == "" || kernel == "" || rootfs == "" {
		t.Skip("set BENCH_HOME, BENCH_KERNEL and BENCH_ROOTFS to benchmark a local image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	d, err := New(ctx, home, artifacts.Paths{Kernel: kernel, Rootfs: rootfs})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := d.Close(); err != nil {
			t.Error(err)
		}
	}()
	var creates, starts []time.Duration
	for i := 0; i < 11; i++ {
		id := fmt.Sprintf("image-bench-%d", i)
		func() {
			begin := time.Now()
			if _, err := d.Create(ctx, id, backend.Spec{CPUs: 1, MemoryMB: 1024, DiskGB: 2}); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := d.Remove(context.Background(), id); err != nil {
					t.Error(err)
				}
			}()
			created := time.Now()
			if err := d.Start(ctx, id); err != nil {
				t.Fatal(err)
			}
			ready := time.Now()
			if i == 0 {
				// Run once, outside the timed sample, only for the full image.
				if os.Getenv("BENCH_AGENTS") == "1" {
					c := agent.NewClient(func(ctx context.Context) (net.Conn, error) { return d.Dial(ctx, id) })
					defer c.CloseIdleConnections()
					var out bytes.Buffer
					s, err := c.Exec(ctx, agent.ExecRequest{Argv: []string{"sh", "-ec", "claude --version; codex --version; gemini --version"}}, &out, &out)
					if err != nil {
						t.Fatal(err)
					}
					code, err := s.Wait()
					t.Log(out.String())
					if err != nil || code != 0 {
						t.Fatalf("agent versions: exit %d, %v", code, err)
					}
				}
				return // Discard the first VM to warm host caches.
			}
			creates = append(creates, created.Sub(begin))
			starts = append(starts, ready.Sub(created))
			t.Logf("sample %d: create=%s start=%s", i, created.Sub(begin), ready.Sub(created))
		}()
	}
	// Use the upper middle value, consistently for both images.
	median := func(x []time.Duration) time.Duration {
		sort.Slice(x, func(i, j int) bool { return x[i] < x[j] })
		return x[len(x)/2]
	}
	t.Logf("create median %s, start median %s, n=%d", median(creates), median(starts), len(starts))
}
