-- A node that joined this head's pool. id is its certificate's common
-- name.
CREATE TABLE nodes (
	id        TEXT PRIMARY KEY,
	name      TEXT NOT NULL,
	os        TEXT NOT NULL,
	arch      TEXT NOT NULL,
	joined_at TEXT NOT NULL
);
