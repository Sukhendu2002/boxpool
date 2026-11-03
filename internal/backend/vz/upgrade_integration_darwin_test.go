//go:build darwin && cgo && integration

package vz

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"boxpool.run/boxpool/internal/artifacts"
)

// Point BOXPOOL_UPGRADE_STATE at state containing a sandbox named legacy,
// paused by a binary using the old direct vmnet attachment. Clone its files
// so verification never consumes or modifies the supplied saved state.
func TestIntegrationUpgradePaused(t *testing.T) {
	fixture := os.Getenv("BOXPOOL_UPGRADE_STATE")
	if fixture == "" {
		t.Skip("set BOXPOOL_UPGRADE_STATE to a pre-filter paused legacy sandbox")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	paths, err := artifacts.Fetch(ctx, filepath.Join(stateDir(t), artifacts.DirName))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	src, dst := filepath.Join(fixture, "sandboxes", "legacy"), filepath.Join(root, "sandboxes", "legacy")
	if err := os.MkdirAll(dst, 0700); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if err := unix.Clonefile(filepath.Join(src, file.Name()), filepath.Join(dst, file.Name()), 0); err != nil {
			t.Fatal(err)
		}
	}
	d, err := New(ctx, root, paths)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := d.Remove(context.Background(), "legacy"); err != nil {
			t.Error(err)
		}
		if err := d.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := d.Resume(ctx, "legacy"); err != nil {
		t.Fatal(err)
	}
	n := &netTest{t, ctx, d}
	// Old saved NIC state cannot carry across attachment classes. Resume is
	// still available over vsock, so a clean stop flushes its dirty memory
	// before a cold start negotiates the new NIC. No snapshot is discarded.
	n.run("legacy", "echo recovered > /root/upgrade-migration")
	if err := d.Stop(ctx, "legacy"); err != nil {
		t.Fatal(err)
	}
	if err := d.Start(ctx, "legacy"); err != nil {
		t.Fatal(err)
	}
	if got := n.run("legacy", "cat /root/upgrade-migration"); got != "recovered\n" {
		t.Fatalf("upgrade stop/start lost data: %q", got)
	}
	out := n.run("legacy", `exec 3<>/dev/tcp/example.com/80 && printf 'HEAD / HTTP/1.0\r\nHost: example.com\r\n\r\n' >&3 && timeout 10 head -n 1 <&3`)
	if !strings.HasPrefix(out, "HTTP/1.") {
		t.Fatalf("upgraded saved NIC: DNS/outbound HTTP: %q", out)
	}
}
