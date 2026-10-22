CREATE TABLE sandboxes (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL UNIQUE,
	backend    TEXT NOT NULL,
	image      TEXT NOT NULL,
	cpus       INTEGER NOT NULL,
	memory_mb  INTEGER NOT NULL,
	disk_gb    INTEGER NOT NULL,
	state      TEXT NOT NULL,
	created_at TEXT NOT NULL
);
