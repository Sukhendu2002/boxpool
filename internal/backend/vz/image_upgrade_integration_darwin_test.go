//go:build darwin && cgo && integration

package vz

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/agent"
	"boxpool.run/boxpool/internal/artifacts"
	"boxpool.run/boxpool/internal/backend"
)

// Supply a verified older compressed rootfs to exercise a real image upgrade
// without modifying an installed daemon, user sandbox or the supplied image.
func TestIntegrationImageUpgrade(t *testing.T) {
	legacy := os.Getenv("BOXPOOL_TEST_LEGACY_ROOTFS")
	if legacy == "" {
		t.Skip("set BOXPOOL_TEST_LEGACY_ROOTFS to a verified older rootfs.ext4.zst")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	paths, err := artifacts.Fetch(ctx, filepath.Join(stateDir(t), artifacts.DirName))
	if err != nil {
		t.Fatal(err)
	}
	old := paths
	old.Rootfs = legacy
	state := t.TempDir()
	d, err := New(ctx, state, old)
	if err != nil {
		t.Fatal(err)
	}
	// Capture the variable so cleanup uses the replacement driver after upgrade.
	t.Cleanup(func() {
		for _, id := range []string{"legacy", "fresh"} {
			if err := d.Remove(context.Background(), id); err != nil {
				t.Error(err)
			}
		}
		if err := d.Close(); err != nil {
			t.Error(err)
		}
	})
	spec := backend.Spec{CPUs: 1, MemoryMB: 512, DiskGB: 3}
	if _, err := d.Create(ctx, "legacy", spec); err != nil {
		t.Fatal(err)
	}
	if err := d.Start(ctx, "legacy"); err != nil {
		t.Fatal(err)
	}
	n := &netTest{t, ctx, d}
	n.run("legacy", "printf 'existing work' > /root/image-upgrade; sync")
	oldClient := d.running("legacy").agent()
	before, err := oldClient.Version(ctx)
	oldClient.CloseIdleConnections()
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Stop(ctx, "legacy"); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	updated, err := New(ctx, state, paths)
	if err != nil {
		t.Fatal(err)
	}
	d = updated
	n.d = d
	if err := d.Start(ctx, "legacy"); err != nil {
		t.Fatal(err)
	}
	if got := n.run("legacy", "cat /root/image-upgrade"); got != "existing work" {
		t.Fatalf("image upgrade lost existing work: %q", got)
	}
	oldClient = d.running("legacy").agent()
	after, err := oldClient.Version(ctx)
	oldClient.CloseIdleConnections()
	if err != nil || after.Agent != before.Agent {
		t.Fatalf("existing disk's agent changed: %+v -> %+v, %v", before, after, err)
	}
	if _, err := d.Create(ctx, "fresh", spec); err != nil {
		t.Fatal(err)
	}
	if err := d.Start(ctx, "fresh"); err != nil {
		t.Fatal(err)
	}
	newClient := d.running("fresh").agent()
	defer newClient.CloseIdleConnections()
	fresh, err := newClient.Version(ctx)
	if err != nil || fresh.Agent == before.Agent || !slices.Contains(fresh.Features, agent.FeatureTCP) || !slices.Contains(fresh.Features, agent.FeatureShutdown) {
		t.Fatalf("fresh disk did not select the new agent: %+v, %v", fresh, err)
	}
	t.Logf("existing sandbox kept agent %s and its data; fresh sandbox uses %s", after.Agent, fresh.Agent)
}
