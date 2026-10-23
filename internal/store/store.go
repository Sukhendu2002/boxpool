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
	ErrNotFound = errors.New("sandbox not found")
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
	id := make([]byte, 6)
	rand.Read(id)
	sb.ID = hex.EncodeToString(id)
	sb.CreatedAt = time.Now().UTC()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO sandboxes (id, name, backend, image, cpus, memory_mb, disk_gb, state, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sb.ID, sb.Spec.Name, sb.Backend, sb.Spec.Image, sb.Spec.CPUs, sb.Spec.MemoryMB, sb.Spec.DiskGB,
		sb.State, sb.CreatedAt.Format(timeFormat))
	var serr *sqlite.Error
	if errors.As(err, &serr) && serr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE {
		return Sandbox{}, fmt.Errorf("sandbox name %q %w", sb.Spec.Name, ErrNameTaken)
	}
	if err != nil {
		return Sandbox{}, err
	}
	return sb, nil
}

const selectSandbox = `SELECT id, name, backend, image, cpus, memory_mb, disk_gb, state, created_at FROM sandboxes`

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
	err := row.Scan(&sb.ID, &sb.Spec.Name, &sb.Backend, &sb.Spec.Image, &sb.Spec.CPUs, &sb.Spec.MemoryMB, &sb.Spec.DiskGB, &sb.State, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Sandbox{}, ErrNotFound
	}
	if err != nil {
		return Sandbox{}, err
	}
	sb.CreatedAt, err = time.Parse(timeFormat, created)
	return sb, err
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
