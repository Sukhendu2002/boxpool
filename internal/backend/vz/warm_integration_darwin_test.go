//go:build darwin && cgo && integration

package vz

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/artifacts"
	"boxpool.run/boxpool/internal/backend/warmtest"
)

func TestIntegrationWarm(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	paths, err := artifacts.Fetch(ctx, filepath.Join(stateDir(t), artifacts.DirName))
	if err != nil {
		t.Fatal(err)
	}
	d, err := New(ctx, stateDir(t), paths)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	warmtest.Check(t, ctx, d, d.isolated, func(id string) bool { return savedState(sandboxLayout(d.root, id)) })
}
