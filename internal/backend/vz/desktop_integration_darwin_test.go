//go:build darwin && cgo && integration

package vz

import (
	"bytes"
	"context"
	"net"
	"os"
	"slices"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/agent"
	"boxpool.run/boxpool/internal/backend"
)

func TestIntegrationDesktop(t *testing.T) {
	t.Setenv("BOXPOOL_DESKTOP_ROOTFS", "")
	d, id := integrationSandboxSpec(t, backend.Spec{Image: "desktop", CPUs: 2, MemoryMB: 2048, DiskGB: 6})
	t.Cleanup(func() {
		if t.Failed() {
			b, _ := os.ReadFile(sandboxLayout(d.root, id).console)
			t.Logf("desktop console:\n%s", b)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := d.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	c := agent.NewClient(func(ctx context.Context) (net.Conn, error) { return d.Dial(ctx, id) })
	defer c.CloseIdleConnections()
	v, err := c.Version(ctx)
	if err != nil || !slices.Contains(v.Features, agent.FeatureDesktop) || !slices.Contains(v.Features, agent.FeatureTCP) {
		t.Fatalf("pinned desktop features: %+v, %v", v, err)
	}
	view, err := c.StartDesktop(ctx)
	if err != nil || view.Display != ":0" || view.HumanControl {
		t.Fatalf("start desktop: %+v, %v", view, err)
	}
	var out bytes.Buffer
	s, err := c.Exec(ctx, agent.ExecRequest{Argv: []string{"sh", "-c", `set -eu
screenshot=$(mktemp "${TMPDIR:-/tmp}/boxpool-desktop.XXXXXX")
trap 'rm -f "$screenshot"' EXIT
# Chromium/Openbox can replace windows while xwd reads their attributes.
# Require window discovery and a nonempty capture in the same bounded retry.
i=0
until DISPLAY=:0 xdotool search --onlyvisible --class chromium >/dev/null 2>&1 \
    && DISPLAY=:0 xwd -root -silent -out "$screenshot" \
    && test -s "$screenshot"; do
    i=$((i+1))
    if test "$i" -ge 100; then
        echo "desktop did not produce a Chromium screenshot" >&2
        exit 1
    fi
    sleep 0.1
done
test "$(tigervncconfig -display :0 -get AcceptKeyEvents)" = 0
test "$(tigervncconfig -display :0 -get AcceptPointerEvents)" = 0
chromium --version`}}, &out, &out)
	if err != nil {
		t.Fatal(err)
	}
	if code, err := s.Wait(); err != nil || code != 0 {
		t.Fatalf("desktop screenshot: %d, %v: %s", code, err, out.String())
	}
	t.Logf("desktop %+v, %s", view, out.String())
}
