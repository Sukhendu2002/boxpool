package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestImageRootfs(t *testing.T) {
	base := filepath.Join(t.TempDir(), "rootfs-arm64.ext4")
	t.Setenv("BOXPOOL_DESKTOP_ROOTFS", "")
	ctx := context.Background()
	calls := 0
	wantErr := errors.New("download failed")
	fetchDesktop := func(gotCtx context.Context, dir string) (string, error) {
		calls++
		if gotCtx != ctx || dir != filepath.Dir(base) {
			t.Fatalf("fetch arguments: %v %s", gotCtx, dir)
		}
		return "", wantErr
	}
	if got, err := imageRootfs(ctx, base, Spec{}, fetchDesktop); err != nil || got != base || calls != 0 {
		t.Fatalf("base: %s %v", got, err)
	}
	if _, err := imageRootfs(ctx, base, Spec{Image: "desktop"}, fetchDesktop); !errors.Is(err, wantErr) || calls != 1 {
		t.Fatalf("desktop download failure: %v, calls %d", err, calls)
	}
	got, err := imageRootfs(ctx, base, Spec{Image: "desktop"}, func(context.Context, string) (string, error) { return "pinned-desktop", nil })
	if err != nil || got != "pinned-desktop" {
		t.Fatalf("pinned desktop: %s %v", got, err)
	}
	desktop := filepath.Join(filepath.Dir(base), "rootfs-desktop.ext4")
	if err := os.WriteFile(desktop, []byte("desktop disk"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BOXPOOL_DESKTOP_ROOTFS", desktop)
	if got, err := ImageRootfs(ctx, base, Spec{Image: "desktop"}); err != nil || got != desktop {
		t.Fatalf("desktop: %s %v", got, err)
	}
	t.Setenv("BOXPOOL_DESKTOP_ROOTFS", filepath.Dir(base))
	if _, err := ImageRootfs(ctx, base, Spec{Image: "desktop"}); err == nil {
		t.Fatal("directory accepted")
	}
	t.Setenv("BOXPOOL_DESKTOP_ROOTFS", filepath.Join(filepath.Dir(base), "missing"))
	if _, err := ImageRootfs(ctx, base, Spec{Image: "desktop"}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing override: %v", err)
	}
	if got, err := ImageRootfs(ctx, base, Spec{Image: "desktop", From: "snapshot"}); err != nil || got != "snapshot" {
		t.Fatalf("snapshot: %s %v", got, err)
	}
}

func TestDesktopNeverUsesBaseWarmMemory(t *testing.T) {
	s, _ := (Spec{Image: "desktop"}).WithDefaults()
	if WarmSpec(s) {
		t.Fatal("desktop would restore base-image memory")
	}
}
