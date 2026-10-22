//go:build linux || darwin

package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"boxpool.run/boxpool/internal/backend"
)

func TestOpenEscapesPath(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	var stores []*Store
	for _, dir := range []string{"state?one", "state?two", "a#b", "c%41d"} {
		path := filepath.Join(root, dir, "state.db")
		if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		s := open(t, path)
		if _, err := s.Create(ctx, Sandbox{Spec: backend.Spec{Name: "web"}}); err != nil {
			t.Fatalf("%s: Create() = %v, want each store to be independent", dir, err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s: %v", dir, err)
		}
		var timeout int
		if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil || timeout != 5000 {
			t.Errorf("%s: busy_timeout = %d, %v; want 5000", dir, timeout, err)
		}
		stores = append(stores, s)
	}
	if entries, _ := os.ReadDir(root); len(entries) != len(stores) {
		t.Errorf("%s has %d entries, want only the %d store directories", root, len(entries), len(stores))
	}
}

func TestOpenPrivateFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.db")
	// A database left readable by an older boxpool is tightened too.
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatal(err)
	}
	s := open(t, path)
	if _, err := s.Create(context.Background(), Sandbox{Spec: backend.Spec{Name: "web"}}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Errorf("%v (WAL mode should keep the sidecar files open)", err)
			continue
		}
		if m := fi.Mode().Perm(); m != 0600 {
			t.Errorf("%s mode = %v, want 0600", filepath.Base(p), m)
		}
	}
}
