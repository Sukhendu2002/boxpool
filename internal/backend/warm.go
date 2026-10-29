package backend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// WarmID is private to the separate warm-cache driver, never the user's store.
const WarmID = "base"

// WarmSpec is the only configuration cached. Other sizes and named disk
// snapshots cold-boot: saved memory must exactly match the disk and VM size.
func WarmSpec(s Spec) bool {
	return s.From == "" && s.CPUs == DefaultCPUs && s.MemoryMB == DefaultMemoryMB && s.DiskGB == DefaultDiskGB
}

// PrepareWarm boots and pauses a private base VM, or reuses its complete cache.
// The separate driver keeps it out of sandbox listings and user reservations.
// Hash the published artifacts, not the mutable disk, to invalidate on upgrades.
func PrepareWarm(ctx context.Context, d Driver, dir, configuration string, quiesce func(context.Context) error, artifacts ...string) error {
	h := sha256.New()
	fmt.Fprintf(h, "boxpool-warm-v2\n%d/%d/%d\n%s\n", DefaultCPUs, DefaultMemoryMB, DefaultDiskGB, configuration)
	for _, path := range artifacts {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(h, f)
		err = errors.Join(err, f.Close())
		if err != nil {
			return err
		}
	}
	key := hex.EncodeToString(h.Sum(nil))
	marker := filepath.Join(dir, "ready")
	if b, err := os.ReadFile(marker); err == nil && string(b) == key {
		list, err := d.List(ctx)
		if err == nil && len(list) == 1 && list[0].ID == WarmID && list[0].State == StatePaused {
			return nil
		}
	}
	if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := d.Remove(ctx, WarmID); err != nil {
		return err
	}
	if _, err := d.Create(ctx, WarmID, Spec{Name: "boxpool-base"}); err != nil {
		return err
	}
	if err := d.Start(ctx, WarmID); err != nil {
		return errors.Join(err, d.Remove(context.WithoutCancel(ctx), WarmID))
	}
	// Freeze the filesystem before saving memory: in-flight disk writes
	// must not race the snapshot or the later copy-on-write disk clone.
	if err := quiesce(ctx); err != nil {
		return errors.Join(err, d.Remove(context.WithoutCancel(ctx), WarmID))
	}
	if err := d.Pause(ctx, WarmID); err != nil {
		return errors.Join(err, d.Remove(context.WithoutCancel(ctx), WarmID))
	}
	if err := os.WriteFile(marker, []byte(key), 0600); err != nil {
		return fmt.Errorf("publish warm base: %w", err)
	}
	return nil
}
