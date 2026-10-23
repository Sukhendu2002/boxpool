-- A sandbox's network slot: the index of its address block in the
-- backend's sandbox subnet.
CREATE TABLE net_slots (
	slot       INTEGER PRIMARY KEY,
	sandbox_id TEXT NOT NULL UNIQUE
);
