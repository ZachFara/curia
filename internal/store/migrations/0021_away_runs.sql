-- F20: an away period. saved_modes is {session id: {mode, until}} as it was
-- before the period; scope is '' (everyone) or a repository; summary is the
-- text `curia back` prints, stored when the period ends so a deadline that
-- ended it unseen still leaves it; shown_at is when the user saw it. Additive.
CREATE TABLE away_runs (
  id INTEGER PRIMARY KEY, started_at TEXT NOT NULL, ready_at TEXT, ends_at TEXT NOT NULL, ended_at TEXT,
  scope TEXT NOT NULL DEFAULT '', saved_modes TEXT, not_ready TEXT, summary TEXT, shown_at TEXT
);
-- F20: 'blocking' when a blocking ticket was stored as assume because its session was away.
ALTER TABLE decisions ADD COLUMN coerced_from TEXT;
