//go:build linux && integration

package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/artifacts"
	"boxpool.run/boxpool/internal/backend/warmtest"
	"boxpool.run/boxpool/internal/store"
)

func TestIntegrationWarm(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
		t.Fatal(err)
	} else {
		f.Close()
	}
	paths, err := artifacts.Fetch(ctx, filepath.Join(stateDir(t), artifacts.DirName))
	if err != nil {
		t.Fatal(err)
	}
	var st *store.Store
	if os.Geteuid() == 0 {
		st, err = store.Open(filepath.Join(stateDir(t), "warm-network.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
	}
	d, err := New(ctx, stateDir(t), paths, st)
	if err != nil {
		t.Fatal(err)
	}
	warmtest.Check(t, ctx, d, st != nil, func(id string) bool { return paused(sandboxLayout(d.root, id)) })
}
