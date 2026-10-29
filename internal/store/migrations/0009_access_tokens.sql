-- An access token for the head's API (#45). Only the SHA-256 of its
-- secret is kept; revoking it deletes its row. sandbox, if set, is the
-- only sandbox it reaches; expires_at and last_used are '' for never.
CREATE TABLE access_tokens (
	hash       TEXT PRIMARY KEY,
	scope      TEXT NOT NULL,
	sandbox    TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL DEFAULT '',
	last_used  TEXT NOT NULL DEFAULT ''
);
