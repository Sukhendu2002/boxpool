package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type warmDriver struct {
	Driver
	state    State
	creates  int
	pauseErr error
}

func (d *warmDriver) Create(context.Context, string, Spec) (Sandbox, error) {
	d.creates++
	d.state = StateStopped
	return Sandbox{}, nil
}
func (d *warmDriver) Start(context.Context, string) error  { d.state = StateRunning; return nil }
func (d *warmDriver) Pause(context.Context, string) error  { d.state = StatePaused; return d.pauseErr }
func (d *warmDriver) Remove(context.Context, string) error { d.state = ""; return nil }
func (d *warmDriver) List(context.Context) ([]Sandbox, error) {
	if d.state == "" {
		return nil, nil
	}
	return []Sandbox{{ID: WarmID, State: d.state}}, nil
}

func TestPrepareWarm(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	artifact := filepath.Join(dir, "artifact")
	if err := os.WriteFile(artifact, []byte("v1"), 0600); err != nil {
		t.Fatal(err)
	}
	d := &warmDriver{}
	freezes := 0
	freeze := func(context.Context) error { freezes++; return nil }
	prepare := func(config string) {
		t.Helper()
		if err := PrepareWarm(ctx, d, dir, config, freeze, artifact); err != nil {
			t.Fatal(err)
		}
	}
	prepare("network=false")
	prepare("network=false")
	if freezes != 1 {
		t.Fatalf("base frozen %d times, want once before caching", freezes)
	}
	if d.creates != 1 {
		t.Fatalf("cache rebuilt: %d creates", d.creates)
	}
	if err := os.WriteFile(artifact, []byte("v2"), 0600); err != nil {
		t.Fatal(err)
	}
	prepare("network=false")
	if d.creates != 2 {
		t.Fatal("image upgrade did not rebuild cache")
	}
	prepare("network=true")
	if d.creates != 3 {
		t.Fatal("configuration change did not rebuild cache")
	}
	d.state = StateStopped
	prepare("network=true")
	if d.creates != 4 {
		t.Fatal("partial cache reused")
	}
	d.state, d.pauseErr = StateStopped, errors.New("save failed")
	if err := PrepareWarm(ctx, d, dir, "network=true", freeze, artifact); !errors.Is(err, d.pauseErr) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ready")); !os.IsNotExist(err) {
		t.Fatal("failed snapshot published ready marker")
	}
	if d.state != "" {
		t.Fatal("failed base VM was not removed")
	}
	d.pauseErr = nil
	freezeErr := errors.New("freeze failed")
	if err := PrepareWarm(ctx, d, dir, "network=true", func(context.Context) error { return freezeErr }, artifact); !errors.Is(err, freezeErr) {
		t.Fatal(err)
	}
	if d.state != "" {
		t.Fatal("unquiesced base was not removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "ready")); !os.IsNotExist(err) {
		t.Fatal("unquiesced base was published")
	}
}

func TestWarmSpec(t *testing.T) {
	spec, _ := (Spec{}).WithDefaults()
	if !WarmSpec(spec) {
		t.Fatal("defaults do not use warm snapshot")
	}
	for _, s := range []Spec{
		{CPUs: 1, MemoryMB: DefaultMemoryMB, DiskGB: DefaultDiskGB},
		{CPUs: DefaultCPUs, MemoryMB: 512, DiskGB: DefaultDiskGB},
		{CPUs: DefaultCPUs, MemoryMB: DefaultMemoryMB, DiskGB: 1},
		{CPUs: DefaultCPUs, MemoryMB: DefaultMemoryMB, DiskGB: DefaultDiskGB, From: "/snapshot"},
	} {
		if WarmSpec(s) {
			t.Errorf("incompatible spec accepted: %+v", s)
		}
	}
}
