//go:build linux && integration

package firecracker

import (
	"context"
	"slices"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/agent"
	"boxpool.run/boxpool/internal/backend"
)

func TestIntegrationDesktop(t *testing.T) {
	t.Setenv("BOXPOOL_DESKTOP_ROOTFS", "")
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	d, id, l, run := newSandbox(t, ctx, backend.Spec{Image: "desktop", CPUs: 2, MemoryMB: 2048, DiskGB: 6})
	if err := d.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	c := d.agent(l)
	defer c.CloseIdleConnections()
	v, err := c.Version(ctx)
	if err != nil || !slices.Contains(v.Features, agent.FeatureDesktop) || !slices.Contains(v.Features, agent.FeatureTCP) {
		t.Fatalf("pinned desktop features: %+v, %v", v, err)
	}
	view, err := c.StartDesktop(ctx)
	if err != nil || view.Display != ":0" || view.HumanControl {
		t.Fatalf("start desktop: %+v, %v", view, err)
	}
	t.Logf("desktop %+v, %s", view, run("DISPLAY=:0 xwd -root -silent > /tmp/screen.xwd && test -s /tmp/screen.xwd && chromium --version"))
}
