-- F20: a permission mode per session (normal | edits | away | bypass), written
-- only by SetModes (never by the session upsert, so a hook update cannot
-- change it). NULL mode: the configured default. mode_until: when it lapses.
ALTER TABLE sessions ADD COLUMN mode TEXT;
ALTER TABLE sessions ADD COLUMN mode_until TEXT;

-- F20: what the hook denied under a mode (allows are not stored: one write per call is too much).
CREATE TABLE permission_events (
  id INTEGER PRIMARY KEY, session_id TEXT NOT NULL, at TEXT NOT NULL, mode TEXT NOT NULL,
  tool TEXT NOT NULL, summary TEXT NOT NULL, verdict TEXT NOT NULL
);
CREATE INDEX permission_events_session ON permission_events (session_id, id);
