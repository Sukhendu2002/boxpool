-- A named snapshot of a sandbox's disk. sandbox is the source's name when
-- it was taken; the source may since be gone.
CREATE TABLE snapshots (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL UNIQUE,
	backend    TEXT NOT NULL,
	sandbox    TEXT NOT NULL,
	disk_gb    INTEGER NOT NULL,
	created_at TEXT NOT NULL
);
