package backend

import (
	"os"
	"path/filepath"
	"testing"
)

func TestImageRootfs(t *testing.T) {
	base := filepath.Join(t.TempDir(), "rootfs-arm64.ext4")
	t.Setenv("BOXPOOL_DESKTOP_ROOTFS", "")
	if got, err := ImageRootfs(base, Spec{}); err != nil || got != base {
		t.Fatalf("base: %s %v", got, err)
	}
	if _, err := ImageRootfs(base, Spec{Image: "desktop"}); err == nil {
		t.Fatal("missing desktop accepted")
	}
	desktop := filepath.Join(filepath.Dir(base), "rootfs-desktop.ext4")
	if err := os.WriteFile(desktop, []byte("desktop disk"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := ImageRootfs(base, Spec{Image: "desktop"}); err != nil || got != desktop {
		t.Fatalf("desktop: %s %v", got, err)
	}
	t.Setenv("BOXPOOL_DESKTOP_ROOTFS", filepath.Dir(base))
	if _, err := ImageRootfs(base, Spec{Image: "desktop"}); err == nil {
		t.Fatal("directory accepted")
	}
	if got, err := ImageRootfs(base, Spec{Image: "desktop", From: "snapshot"}); err != nil || got != "snapshot" {
		t.Fatalf("snapshot: %s %v", got, err)
	}
}

func TestDesktopNeverUsesBaseWarmMemory(t *testing.T) {
	s, _ := (Spec{Image: "desktop"}).WithDefaults()
	if WarmSpec(s) {
		t.Fatal("desktop would restore base-image memory")
	}
}
