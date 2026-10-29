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

	web, err := s.Create(ctx, Sandbox{Backend: "firecracker", Spec: spec, State: backend.StateRunning, Restart: "always"})
	if err != nil {
		t.Fatal(err)
	}
	if len(web.ID) != 12 || web.CreatedAt.IsZero() || web.CreatedAt.Location() != time.UTC {
		t.Errorf("Create() = %+v, want a 12-char ID and a UTC creation time", web)
	}
	if got, err := s.Get(ctx, web.ID); err != nil || !got.CreatedAt.Equal(web.CreatedAt) || got.Spec != spec || got.Backend != "firecracker" || got.State != backend.StateRunning || got.Restart != "always" {
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
	if db.Spec.CPUs != 2 || db.Spec.MemoryMB != 2048 || db.Spec.DiskGB != 10 {
		t.Errorf("stored defaults: %+v", db.Spec)
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

func TestFreeSlot(t *testing.T) {
	for _, tc := range []struct {
		used    []int
		n, want int
	}{
		{nil, 3, 0},
		{[]int{0, 1}, 3, 2},
		{[]int{0, 2}, 3, 1}, // a freed slot is reused before a new one
		{[]int{1, 2}, 3, 0},
	} {
		if got, err := freeSlot(tc.used, tc.n); err != nil || got != tc.want {
			t.Errorf("freeSlot(%v, %d) = %d, %v; want %d", tc.used, tc.n, got, err, tc.want)
		}
	}
	if _, err := freeSlot([]int{0, 1, 2}, 3); !errors.Is(err, ErrNoSlots) {
		t.Errorf("freeSlot of a full range = %v, want ErrNoSlots", err)
	}
}

func TestNetSlot(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	slot := func(id string) int {
		t.Helper()
		n, err := s.NetSlot(ctx, id, 2)
		if err != nil {
			t.Fatalf("NetSlot(%s) = %v", id, err)
		}
		return n
	}
	if a, b := slot("a"), slot("b"); a != 0 || b != 1 {
		t.Errorf("slots of a and b = %d, %d; want 0, 1", a, b)
	}
	if a := slot("a"); a != 0 {
		t.Errorf("second NetSlot(a) = %d, want its slot 0", a)
	}
	if _, err := s.NetSlot(ctx, "c", 2); !errors.Is(err, ErrNoSlots) {
		t.Errorf("NetSlot with every slot taken = %v, want ErrNoSlots", err)
	}
	if err := s.FreeNetSlot(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.FreeNetSlot(ctx, "a"); err != nil {
		t.Errorf("second FreeNetSlot(a) = %v", err)
	}
	if c := slot("c"); c != 0 {
		t.Errorf("NetSlot(c) after freeing a = %d, want a's slot 0", c)
	}
	if got, err := s.NetSlots(ctx); err != nil || len(got) != 2 || got["b"] != 1 || got["c"] != 0 {
		t.Errorf("NetSlots() = %v, %v; want b:1 c:0", got, err)
	}
}

func TestSnapshots(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	jq, err := s.CreateSnapshot(ctx, Snapshot{Name: "withjq", Backend: "vz", Sandbox: "box", DiskGB: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(jq.ID) != 12 || jq.CreatedAt.IsZero() {
		t.Errorf("CreateSnapshot() = %+v, want a 12-char ID and a creation time", jq)
	}
	if got, err := s.GetSnapshot(ctx, "withjq"); err != nil || got != jq {
		t.Errorf("GetSnapshot() = %+v, %v; want %+v", got, err, jq)
	}
	if _, err := s.CreateSnapshot(ctx, Snapshot{Name: "withjq", Backend: "vz"}); !errors.Is(err, ErrNameTaken) || err.Error() != `snapshot name "withjq" already exists` {
		t.Errorf("duplicate CreateSnapshot() = %v, want ErrNameTaken", err)
	}
	// A sandbox may share a snapshot's name.
	if _, err := s.Create(ctx, Sandbox{Spec: backend.Spec{Name: "withjq"}}); err != nil {
		t.Errorf("Create() with a snapshot's name = %v", err)
	}
	py, err := s.CreateSnapshot(ctx, Snapshot{Name: "withpy", Backend: "vz", Sandbox: "box", DiskGB: 10})
	if err != nil {
		t.Fatal(err)
	}
	if list, err := s.ListSnapshots(ctx); err != nil || len(list) != 2 || list[0] != jq || list[1] != py {
		t.Errorf("ListSnapshots() = %+v, %v; want withjq then withpy", list, err)
	}
	if err := s.DeleteSnapshot(ctx, jq.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSnapshot(ctx, "withjq"); !errors.Is(err, ErrSnapshotNotFound) {
		t.Errorf("GetSnapshot() after delete = %v, want ErrSnapshotNotFound", err)
	}
	if err := s.DeleteSnapshot(ctx, jq.ID); !errors.Is(err, ErrSnapshotNotFound) {
		t.Errorf("second DeleteSnapshot() = %v, want ErrSnapshotNotFound", err)
	}
}

func TestJoinTokens(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	t0 := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	for _, h := range []string{"once", "late", "revoked"} {
		if err := s.CreateJoinToken(ctx, h, t0.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UseJoinToken(ctx, "once", t0.Add(59*time.Minute)); err != nil {
		t.Errorf("first UseJoinToken() = %v", err)
	}
	if err := s.UseJoinToken(ctx, "once", t0); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("second UseJoinToken() = %v, want ErrTokenInvalid", err)
	}
	if err := s.UseJoinToken(ctx, "late", t0.Add(time.Hour)); !errors.Is(err, ErrTokenExpired) || err.Error() != "token expired" {
		t.Errorf("UseJoinToken() at expiry = %v, want ErrTokenExpired", err)
	}
	if err := s.UseJoinToken(ctx, "unknown", t0); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("UseJoinToken() of an unknown hash = %v, want ErrTokenInvalid", err)
	}
	if err := s.RevokeJoinToken(ctx, "revoked"); err != nil {
		t.Errorf("RevokeJoinToken() = %v", err)
	}
	if err := s.UseJoinToken(ctx, "revoked", t0); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("UseJoinToken() after revoke = %v, want ErrTokenInvalid", err)
	}
	if err := s.RevokeJoinToken(ctx, "revoked"); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("second RevokeJoinToken() = %v, want ErrTokenInvalid", err)
	}
	// Issuing another token must preserve the expiry error for an unused token.
	if err := s.CreateJoinToken(ctx, "stale", t0.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateJoinToken(ctx, "fresh", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.UseJoinToken(ctx, "stale", t0); !errors.Is(err, ErrTokenExpired) {
		t.Errorf("UseJoinToken() after creating another token = %v, want ErrTokenExpired", err)
	}
}

func TestNodes(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	mac, err := s.CreateNode(ctx, Node{Name: "mac", OS: "darwin", Arch: "arm64"})
	if err != nil {
		t.Fatal(err)
	}
	if len(mac.ID) != 12 || mac.JoinedAt.IsZero() {
		t.Errorf("CreateNode() = %+v, want a 12-char ID and a join time", mac)
	}
	box, err := s.CreateNode(ctx, Node{Name: "box", OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if list, err := s.ListNodes(ctx); err != nil || len(list) != 2 || list[0] != mac || list[1] != box {
		t.Errorf("ListNodes() = %+v, %v; want mac then box", list, err)
	}
}
