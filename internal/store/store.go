// Package store keeps the node's sandbox records in a SQLite database in the
// state directory, so the daemon knows what it created after a restart.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"boxpool.run/boxpool/internal/backend"
)

var (
	ErrNotFound         = errors.New("sandbox not found")
	ErrSnapshotNotFound = errors.New("snapshot not found")
	// ErrNameTaken is wrapped as `sandbox name "web" already exists`.
	ErrNameTaken = errors.New("already exists")
)

// timeFormat is fixed-width so that created_at sorts as text.
const timeFormat = "2006-01-02T15:04:05.000000000Z07:00"

//go:embed migrations/*.sql
var migrations embed.FS

// Sandbox is a sandbox record. Spec.Name is the sandbox's name, unique on
// this node.
type Sandbox struct {
	ID        string
	Backend   string
	Spec      backend.Spec
	State     backend.State
	CreatedAt time.Time
	// Restart is the restart policy: "always" or "" for none.
	Restart string
}

// Store is the node's sandbox database.
type Store struct {
	db *sql.DB
}

// Open opens or creates the database at path and migrates it to the latest
// schema.
func Open(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// SQLite gives the -wal and -shm files the database file's mode, so
	// creating and tightening it first keeps all three private.
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	for _, p := range []string{abs, abs + "-wal", abs + "-shm"} {
		if err := os.Chmod(p, 0600); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	// An escaped file: URL keeps '?', '#' and '%' in the path from being
	// read as the query string.
	dsn := url.URL{Scheme: "file", Path: abs, RawQuery: url.Values{
		"_pragma": {"busy_timeout(5000)", "journal_mode(WAL)", "foreign_keys(1)"},
	}.Encode()}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, err
	}
	// SQLite 3.51.2 (modernc.org/sqlite v1.46.1) can corrupt a WAL database
	// when writes and checkpoints run on separate connections
	// (https://www.sqlite.org/wal.html#walreset), so use one connection.
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// migrate applies migrations/NNNN_*.sql in order, each in its own
// transaction, and records the schema version in PRAGMA user_version.
func migrate(db *sql.DB) error {
	files, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	var current int
	if err := db.QueryRow("PRAGMA user_version").Scan(&current); err != nil {
		return err
	}
	// A downgraded binary must not write to a schema it does not know.
	if current > len(files) {
		return fmt.Errorf("database schema version %d is newer than this boxpool supports (%d): upgrade boxpool", current, len(files))
	}
	for i, f := range files[current:] {
		v := current + i + 1
		if want := fmt.Sprintf("%04d_", v); !strings.HasPrefix(f.Name(), want) {
			return fmt.Errorf("migration %s: want a name starting with %s", f.Name(), want)
		}
		q, err := migrations.ReadFile("migrations/" + f.Name())
		if err != nil {
			return err
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(q)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", f.Name(), err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Create stores sb under a new ID and returns it with ID and CreatedAt set.
func (s *Store) Create(ctx context.Context, sb Sandbox) (Sandbox, error) {
	var err error
	if sb.Spec, err = sb.Spec.WithDefaults(); err != nil {
		return Sandbox{}, err
	}
	sb.ID, sb.CreatedAt = NewID(), time.Now().UTC()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO sandboxes (id, name, backend, image, cpus, memory_mb, disk_gb, state, created_at, restart)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sb.ID, sb.Spec.Name, sb.Backend, sb.Spec.Image, sb.Spec.CPUs, sb.Spec.MemoryMB, sb.Spec.DiskGB,
		sb.State, sb.CreatedAt.Format(timeFormat), sb.Restart)
	var serr *sqlite.Error
	if errors.As(err, &serr) && serr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE {
		return Sandbox{}, fmt.Errorf("sandbox name %q %w", sb.Spec.Name, ErrNameTaken)
	}
	if err != nil {
		return Sandbox{}, err
	}
	return sb, nil
}

const selectSandbox = `SELECT id, name, backend, image, cpus, memory_mb, disk_gb, state, created_at, restart FROM sandboxes`

// Get returns the sandbox with the given ID.
func (s *Store) Get(ctx context.Context, id string) (Sandbox, error) {
	return scan(s.db.QueryRowContext(ctx, selectSandbox+` WHERE id = ?`, id))
}

// GetByName returns the sandbox with the given name.
func (s *Store) GetByName(ctx context.Context, name string) (Sandbox, error) {
	return scan(s.db.QueryRowContext(ctx, selectSandbox+` WHERE name = ?`, name))
}

// List returns all sandboxes, oldest first.
func (s *Store) List(ctx context.Context) ([]Sandbox, error) {
	rows, err := s.db.QueryContext(ctx, selectSandbox+` ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Sandbox
	for rows.Next() {
		sb, err := scan(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, sb)
	}
	return list, rows.Err()
}

// SetState updates the state of the sandbox with the given ID.
func (s *Store) SetState(ctx context.Context, id string, state backend.State) error {
	return expectOne(s.db.ExecContext(ctx, `UPDATE sandboxes SET state = ? WHERE id = ?`, state, id))
}

// Delete removes the sandbox with the given ID.
func (s *Store) Delete(ctx context.Context, id string) error {
	return expectOne(s.db.ExecContext(ctx, `DELETE FROM sandboxes WHERE id = ?`, id))
}

func expectOne(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

func scan(row interface{ Scan(...any) error }) (Sandbox, error) {
	var sb Sandbox
	var created string
	err := row.Scan(&sb.ID, &sb.Spec.Name, &sb.Backend, &sb.Spec.Image, &sb.Spec.CPUs, &sb.Spec.MemoryMB, &sb.Spec.DiskGB, &sb.State, &created, &sb.Restart)
	if errors.Is(err, sql.ErrNoRows) {
		return Sandbox{}, ErrNotFound
	}
	if err != nil {
		return Sandbox{}, err
	}
	sb.CreatedAt, err = time.Parse(timeFormat, created)
	return sb, err
}

func NewID() string {
	id := make([]byte, 6)
	rand.Read(id)
	return hex.EncodeToString(id)
}

// Snapshot is a named copy of a sandbox's disk, which new sandboxes on the
// same backend can start from. Sandbox is the source's name when it was
// taken.
type Snapshot struct {
	ID        string
	Name      string
	Backend   string
	Sandbox   string
	DiskGB    int
	CreatedAt time.Time
}

// CreateSnapshot stores snap under a new ID and returns it with ID and
// CreatedAt set.
func (s *Store) CreateSnapshot(ctx context.Context, snap Snapshot) (Snapshot, error) {
	snap.ID, snap.CreatedAt = NewID(), time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO snapshots (id, name, backend, sandbox, disk_gb, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		snap.ID, snap.Name, snap.Backend, snap.Sandbox, snap.DiskGB, snap.CreatedAt.Format(timeFormat))
	var serr *sqlite.Error
	if errors.As(err, &serr) && serr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE {
		return Snapshot{}, fmt.Errorf("snapshot name %q %w", snap.Name, ErrNameTaken)
	}
	if err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

const selectSnapshot = `SELECT id, name, backend, sandbox, disk_gb, created_at FROM snapshots`

// GetSnapshot returns the snapshot with the given name.
func (s *Store) GetSnapshot(ctx context.Context, name string) (Snapshot, error) {
	return scanSnapshot(s.db.QueryRowContext(ctx, selectSnapshot+` WHERE name = ?`, name))
}

// ListSnapshots returns all snapshots, oldest first.
func (s *Store) ListSnapshots(ctx context.Context) ([]Snapshot, error) {
	rows, err := s.db.QueryContext(ctx, selectSnapshot+` ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Snapshot
	for rows.Next() {
		snap, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, snap)
	}
	return list, rows.Err()
}

// DeleteSnapshot removes the snapshot with the given ID.
func (s *Store) DeleteSnapshot(ctx context.Context, id string) error {
	err := expectOne(s.db.ExecContext(ctx, `DELETE FROM snapshots WHERE id = ?`, id))
	if errors.Is(err, ErrNotFound) {
		return ErrSnapshotNotFound
	}
	return err
}

func scanSnapshot(row interface{ Scan(...any) error }) (Snapshot, error) {
	var snap Snapshot
	var created string
	err := row.Scan(&snap.ID, &snap.Name, &snap.Backend, &snap.Sandbox, &snap.DiskGB, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, ErrSnapshotNotFound
	}
	if err != nil {
		return Snapshot{}, err
	}
	snap.CreatedAt, err = time.Parse(timeFormat, created)
	return snap, err
}

// ErrNoSlots is returned by NetSlot when every slot is taken.
var ErrNoSlots = errors.New("no free network slots")

// NetSlot returns the network slot of the sandbox with the given ID,
// allocating the lowest free one below n if it has none. The ID need not be
// a stored sandbox's: the backend owns slots, and frees them on remove.
func (s *Store) NetSlot(ctx context.Context, sandboxID string, n int) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var slot int
	err = tx.QueryRowContext(ctx, `SELECT slot FROM net_slots WHERE sandbox_id = ?`, sandboxID).Scan(&slot)
	if err == nil {
		return slot, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT slot FROM net_slots ORDER BY slot`)
	if err != nil {
		return 0, err
	}
	var used []int
	for rows.Next() {
		if err := rows.Scan(&slot); err != nil {
			rows.Close()
			return 0, err
		}
		used = append(used, slot)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if slot, err = freeSlot(used, n); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO net_slots (slot, sandbox_id) VALUES (?, ?)`, slot, sandboxID); err != nil {
		return 0, err
	}
	return slot, tx.Commit()
}

// freeSlot returns the lowest slot below n that is not in used, which is
// sorted.
func freeSlot(used []int, n int) (int, error) {
	for i, u := range used {
		if u != i {
			return i, nil
		}
	}
	if len(used) >= n {
		return 0, fmt.Errorf("%w: all %d are taken", ErrNoSlots, n)
	}
	return len(used), nil
}

// FreeNetSlot frees the network slot of the sandbox with the given ID, if it
// has one.
func (s *Store) FreeNetSlot(ctx context.Context, sandboxID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM net_slots WHERE sandbox_id = ?`, sandboxID)
	return err
}

// NetSlots returns every allocated slot by sandbox ID.
func (s *Store) NetSlots(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sandbox_id, slot FROM net_slots`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	slots := map[string]int{}
	for rows.Next() {
		var id string
		var slot int
		if err := rows.Scan(&id, &slot); err != nil {
			return nil, err
		}
		slots[id] = slot
	}
	return slots, rows.Err()
}

var (
	ErrTokenInvalid = errors.New("token is invalid or already used")
	ErrTokenExpired = errors.New("token expired")
)

// CreateJoinToken stores the hash of a join token's secret, valid until
// expires. Expired hashes are kept until use or revocation so that an
// unused expired token can be distinguished from an invalid token.
func (s *Store) CreateJoinToken(ctx context.Context, hash string, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO join_tokens (hash, expires_at) VALUES (?, ?)`, hash, expires.UTC().Format(timeFormat))
	return err
}

// UseJoinToken consumes the join token with the given hash: it succeeds
// once, before the token expires at the time now.
func (s *Store) UseJoinToken(ctx context.Context, hash string, now time.Time) error {
	var expires string
	err := s.db.QueryRowContext(ctx, `DELETE FROM join_tokens WHERE hash = ? RETURNING expires_at`, hash).Scan(&expires)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrTokenInvalid
	}
	if err != nil {
		return err
	}
	t, err := time.Parse(timeFormat, expires)
	if err != nil {
		return err
	}
	if !now.Before(t) {
		return ErrTokenExpired
	}
	return nil
}

// RevokeJoinToken deletes the unused join token with the given hash.
func (s *Store) RevokeJoinToken(ctx context.Context, hash string) error {
	err := expectOne(s.db.ExecContext(ctx, `DELETE FROM join_tokens WHERE hash = ?`, hash))
	if errors.Is(err, ErrNotFound) {
		return ErrTokenInvalid
	}
	return err
}

// Node is a machine that joined this head's pool.
type Node struct {
	ID       string
	Name     string
	OS       string
	Arch     string
	JoinedAt time.Time
	// Report is the node's last heartbeat as it sent it, and SeenAt when it
	// came: zero before the first.
	Report []byte
	SeenAt time.Time
}

// CreateNode stores n, under a new ID and join time unless it has an ID,
// and returns it with ID and JoinedAt set.
func (s *Store) CreateNode(ctx context.Context, n Node) (Node, error) {
	if n.ID == "" {
		n.ID, n.JoinedAt = NewID(), time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO nodes (id, name, os, arch, joined_at) VALUES (?, ?, ?, ?, ?)`,
		n.ID, n.Name, n.OS, n.Arch, n.JoinedAt.Format(timeFormat))
	if err != nil {
		return Node{}, err
	}
	return n, nil
}

// ListNodes returns every joined node, oldest first.
func (s *Store) ListNodes(ctx context.Context) ([]Node, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, os, arch, joined_at, report, seen_at FROM nodes ORDER BY joined_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Node
	for rows.Next() {
		var n Node
		var joined, report, seen string
		if err := rows.Scan(&n.ID, &n.Name, &n.OS, &n.Arch, &joined, &report, &seen); err != nil {
			return nil, err
		}
		if n.JoinedAt, err = time.Parse(timeFormat, joined); err != nil {
			return nil, err
		}
		if seen != "" {
			if n.SeenAt, err = time.Parse(timeFormat, seen); err != nil {
				return nil, err
			}
			n.Report = []byte(report)
		}
		list = append(list, n)
	}
	return list, rows.Err()
}

// SetNodeReport records the node's heartbeat report, received at seen. It
// returns ErrNotFound for an unknown node.
func (s *Store) SetNodeReport(ctx context.Context, id string, report []byte, seen time.Time) error {
	return expectOne(s.db.ExecContext(ctx, `UPDATE nodes SET report = ?, seen_at = ? WHERE id = ?`,
		string(report), seen.UTC().Format(timeFormat), id))
}
