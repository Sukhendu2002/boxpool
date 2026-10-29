package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/hashicorp/raft"
)

// RaftStore is raft's LogStore and StableStore in the store's database, on
// a machine that keeps a replica of the pool's state.
type RaftStore struct{ db *sql.DB }

var (
	_ raft.LogStore    = RaftStore{}
	_ raft.StableStore = RaftStore{}
)

// Raft returns the store's raft log and stable state.
func (s *Store) Raft() RaftStore { return RaftStore{s.db} }

// FirstIndex returns the first raft log index, 0 when the log is empty.
func (s RaftStore) FirstIndex() (uint64, error) { return s.raftIndex(`SELECT MIN(idx) FROM raft_log`) }

// LastIndex returns the last raft log index, 0 when the log is empty.
func (s RaftStore) LastIndex() (uint64, error) { return s.raftIndex(`SELECT MAX(idx) FROM raft_log`) }

func (s RaftStore) raftIndex(q string) (uint64, error) {
	var i sql.NullInt64
	err := s.db.QueryRow(q).Scan(&i)
	return uint64(i.Int64), err
}

// GetLog reads the raft log entry at index. AppendedAt, which raft only
// uses for metrics, is not kept.
func (s RaftStore) GetLog(index uint64, l *raft.Log) error {
	var typ int64
	err := s.db.QueryRow(`SELECT term, type, data, extensions FROM raft_log WHERE idx = ?`, int64(index)).Scan(&l.Term, &typ, &l.Data, &l.Extensions)
	if errors.Is(err, sql.ErrNoRows) {
		return raft.ErrLogNotFound
	}
	l.Index, l.Type = index, raft.LogType(typ)
	return err
}

// StoreLog stores a raft log entry.
func (s RaftStore) StoreLog(l *raft.Log) error { return s.StoreLogs([]*raft.Log{l}) }

// StoreLogs stores raft log entries, replacing any at the same indexes.
func (s RaftStore) StoreLogs(logs []*raft.Log) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, l := range logs {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO raft_log (idx, term, type, data, extensions) VALUES (?, ?, ?, ?, ?)`,
			int64(l.Index), int64(l.Term), int64(l.Type), l.Data, l.Extensions); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteRange deletes the raft log entries from min to max inclusive.
func (s RaftStore) DeleteRange(min, max uint64) error {
	_, err := s.db.Exec(`DELETE FROM raft_log WHERE idx BETWEEN ? AND ?`, int64(min), int64(max))
	return err
}

// errKeyNotFound is the error raft expects, by its text, for a missing key.
var errKeyNotFound = errors.New("not found")

// Set stores a raft stable value.
func (s RaftStore) Set(key, val []byte) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO raft_stable (key, value) VALUES (?, ?)`, string(key), val)
	return err
}

// Get returns a raft stable value.
func (s RaftStore) Get(key []byte) ([]byte, error) {
	var val []byte
	err := s.db.QueryRow(`SELECT value FROM raft_stable WHERE key = ?`, string(key)).Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errKeyNotFound
	}
	return val, err
}

// SetUint64 stores a raft stable number.
func (s RaftStore) SetUint64(key []byte, val uint64) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO raft_stable (key, value) VALUES (?, ?)`, string(key), int64(val))
	return err
}

// GetUint64 returns a raft stable number.
func (s RaftStore) GetUint64(key []byte) (uint64, error) {
	var val int64
	err := s.db.QueryRow(`SELECT value FROM raft_stable WHERE key = ?`, string(key)).Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errKeyNotFound
	}
	return uint64(val), err
}

// ResetRaft deletes the raft log and stable state.
func (s RaftStore) ResetRaft() error {
	_, err := s.db.Exec(`DELETE FROM raft_log; DELETE FROM raft_stable`)
	return err
}

// JoinToken is an unused join token: the hash of its secret, and when it
// expires.
type JoinToken struct {
	Hash      string
	ExpiresAt time.Time
}

// PoolState is the head's state that raft replicates: the pool's nodes,
// with their last reports, and its unused join tokens.
type PoolState struct {
	Nodes  []Node
	Tokens []JoinToken
}

// PoolState returns the nodes and unused join tokens.
func (s *Store) PoolState(ctx context.Context) (PoolState, error) {
	nodes, err := s.ListNodes(ctx)
	if err != nil {
		return PoolState{}, err
	}
	state := PoolState{Nodes: nodes}
	rows, err := s.db.QueryContext(ctx, `SELECT hash, expires_at FROM join_tokens ORDER BY hash`)
	if err != nil {
		return PoolState{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var tok JoinToken
		var expires string
		if err := rows.Scan(&tok.Hash, &expires); err != nil {
			return PoolState{}, err
		}
		if tok.ExpiresAt, err = time.Parse(timeFormat, expires); err != nil {
			return PoolState{}, err
		}
		state.Tokens = append(state.Tokens, tok)
	}
	return state, rows.Err()
}

// SetPoolState replaces the nodes and join tokens with state's.
func (s *Store) SetPoolState(ctx context.Context, state PoolState) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM nodes; DELETE FROM join_tokens`); err != nil {
		return err
	}
	for _, n := range state.Nodes {
		var seen string
		if !n.SeenAt.IsZero() {
			seen = n.SeenAt.UTC().Format(timeFormat)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO nodes (id, name, os, arch, joined_at, report, seen_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			n.ID, n.Name, n.OS, n.Arch, n.JoinedAt.UTC().Format(timeFormat), string(n.Report), seen); err != nil {
			return err
		}
	}
	for _, tok := range state.Tokens {
		if _, err := tx.ExecContext(ctx, `INSERT INTO join_tokens (hash, expires_at) VALUES (?, ?)`, tok.Hash, tok.ExpiresAt.UTC().Format(timeFormat)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
