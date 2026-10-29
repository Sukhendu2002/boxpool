-- Raft's log and stable state, on a machine that keeps a copy of the
-- pool's state (#41).
CREATE TABLE raft_log (
	idx        INTEGER PRIMARY KEY,
	term       INTEGER NOT NULL,
	type       INTEGER NOT NULL,
	data       BLOB,
	extensions BLOB
);
CREATE TABLE raft_stable (
	key   TEXT PRIMARY KEY,
	value BLOB NOT NULL
);
