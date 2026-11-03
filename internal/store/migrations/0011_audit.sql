-- Audit targets and actor IDs survive deletion of the objects they name.
CREATE TABLE audit_events (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	time       TEXT NOT NULL,
	token_id   TEXT NOT NULL,
	action     TEXT NOT NULL,
	target     TEXT NOT NULL
);
CREATE INDEX audit_events_time ON audit_events(time);
