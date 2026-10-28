-- A sandbox's restart policy: 'always' boots it again when the daemon
-- starts and finds it stopped though it was running.
ALTER TABLE sandboxes ADD COLUMN restart TEXT NOT NULL DEFAULT '';
