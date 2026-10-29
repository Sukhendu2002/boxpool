-- A join token the head issued. Only the SHA-256 of its secret is kept;
-- using the token deletes its row, so it works once.
CREATE TABLE join_tokens (
	hash       TEXT PRIMARY KEY,
	expires_at TEXT NOT NULL
);
