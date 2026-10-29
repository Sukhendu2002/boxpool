-- A node's last heartbeat: its report as JSON, and when the head got it.
-- Both are kept while the node is offline.
ALTER TABLE nodes ADD COLUMN report TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN seen_at TEXT NOT NULL DEFAULT '';
