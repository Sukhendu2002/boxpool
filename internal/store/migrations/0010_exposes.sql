-- A sandbox port boxpool expose serves on the head (#46): url is
-- https://<tailnet name>:<port>/, which tailscale serve sends to
-- 127.0.0.1:<port>. token_id is the expose token made for it, '' if public.
CREATE TABLE exposes (
	port         INTEGER PRIMARY KEY,
	sandbox      TEXT NOT NULL,
	sandbox_port INTEGER NOT NULL,
	public       INTEGER NOT NULL,
	token_id     TEXT NOT NULL DEFAULT '',
	url          TEXT NOT NULL,
	created_at   TEXT NOT NULL,
	UNIQUE (sandbox, sandbox_port)
);
