package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/backend"
)

func open(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func userVersion(t *testing.T, s *Store) int {
	t.Helper()
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestMigrate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s := open(t, path)
	files, _ := migrations.ReadDir("migrations")
	if v := userVersion(t, s); v != len(files) || v == 0 {
		t.Fatalf("user_version = %d, want %d", v, len(files))
	}
	ctx := context.Background()
	if _, err := s.Create(ctx, Sandbox{Spec: backend.Spec{Name: "web"}}); err != nil {
		t.Fatal(err)
	}
	// Migrating again must not re-run a migration (CREATE TABLE would fail).
	if err := migrate(s.db); err != nil {
		t.Fatalf("second migrate = %v", err)
	}
	if v := userVersion(t, s); v != len(files) {
		t.Errorf("user_version after second migrate = %d, want %d", v, len(files))
	}
	if list, err := s.List(ctx); err != nil || len(list) != 1 {
		t.Errorf("List() after second migrate = %v, %v", list, err)
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if s, err := Open(path); err == nil || !strings.Contains(err.Error(), "newer than this boxpool supports") {
		if s != nil {
			s.Close()
		}
		t.Errorf("Open() = %v, want a refusal", err)
	}
}

func TestRepository(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	spec := backend.Spec{Name: "web", Image: "base", CPUs: 2, MemoryMB: 2048, DiskGB: 10}

	web, err := s.Create(ctx, Sandbox{Backend: "firecracker", Spec: spec, State: backend.StateRunning})
	if err != nil {
		t.Fatal(err)
	}
	if len(web.ID) != 12 || web.CreatedAt.IsZero() || web.CreatedAt.Location() != time.UTC {
		t.Errorf("Create() = %+v, want a 12-char ID and a UTC creation time", web)
	}
	if got, err := s.Get(ctx, web.ID); err != nil || !got.CreatedAt.Equal(web.CreatedAt) || got.Spec != spec || got.Backend != "firecracker" || got.State != backend.StateRunning {
		t.Errorf("Get() = %+v, %v; want %+v", got, err, web)
	}
	if got, err := s.GetByName(ctx, "web"); err != nil || got.ID != web.ID {
		t.Errorf("GetByName() = %+v, %v; want ID %s", got, err, web.ID)
	}

	_, err = s.Create(ctx, Sandbox{Backend: "docker", Spec: spec, State: backend.StateStopped})
	if !errors.Is(err, ErrNameTaken) || err.Error() != `sandbox name "web" already exists` {
		t.Errorf("duplicate Create() = %v, want ErrNameTaken", err)
	}

	db, err := s.Create(ctx, Sandbox{Backend: "firecracker", Spec: backend.Spec{Name: "db"}, State: backend.StateRunning})
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.List(ctx)
	if err != nil || len(list) != 2 || list[0].ID != web.ID || list[1].ID != db.ID {
		t.Errorf("List() = %+v, %v; want web then db", list, err)
	}

	if err := s.SetState(ctx, web.ID, backend.StatePaused); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, web.ID); got.State != backend.StatePaused {
		t.Errorf("state after SetState = %q, want paused", got.State)
	}

	if err := s.Delete(ctx, web.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, web.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get() after Delete = %v, want ErrNotFound", err)
	}
	// The name is free again.
	if _, err := s.Create(ctx, Sandbox{Spec: backend.Spec{Name: "web"}}); err != nil {
		t.Errorf("Create() reusing a deleted name = %v", err)
	}

	for name, err := range map[string]error{
		"GetByName": func() error { _, err := s.GetByName(ctx, "missing"); return err }(),
		"SetState":  s.SetState(ctx, "missing", backend.StateStopped),
		"Delete":    s.Delete(ctx, "missing"),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s(missing) = %v, want ErrNotFound", name, err)
		}
	}
}
