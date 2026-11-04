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
	t.Logf("desktop %+v, %s", view, run(`set -eu
i=0
until DISPLAY=:0 xdotool search --onlyvisible --class chromium 2>/dev/null; do
    i=$((i+1)); test "$i" -lt 100; sleep 0.1
done
DISPLAY=:0 xwd -root -silent > /tmp/screen.xwd
test -s /tmp/screen.xwd
test "$(tigervncconfig -display :0 -get AcceptKeyEvents)" = 0
test "$(tigervncconfig -display :0 -get AcceptPointerEvents)" = 0
chromium --version`))
}
