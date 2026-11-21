-- F22 scheduled messages (Ruling 151): what to send, to whom, when; and one row per fire.
CREATE TABLE schedules (
  id           TEXT PRIMARY KEY,                -- S-3
  text         TEXT NOT NULL,
  kind         TEXT NOT NULL CHECK (kind IN ('at','daily','on_reset')),
  spec         TEXT NOT NULL,                   -- the time as given: RFC 3339, HH:MM, or '' for on_reset
  target       TEXT NOT NULL,                   -- 'all' | 'repo:<name>' | 'task:T-42'
  if_idle      INTEGER NOT NULL DEFAULT 0,
  only_limited INTEGER NOT NULL DEFAULT 0,
  status       TEXT NOT NULL CHECK (status IN ('active','paused','done','canceled')),
  next_run_at  TEXT,                            -- NULL once done or canceled
  created_via  TEXT NOT NULL DEFAULT 'cli',
  created_at   TEXT NOT NULL
);
CREATE INDEX schedules_due ON schedules(next_run_at) WHERE status = 'active';
CREATE TABLE schedule_runs (
  id          INTEGER PRIMARY KEY,
  schedule_id TEXT NOT NULL,
  due_at      TEXT NOT NULL,
  ran_at      TEXT NOT NULL,
  outcome     TEXT NOT NULL CHECK (outcome IN ('fired','skipped_late','skipped_no_target','monitor_down')),
  results     TEXT NOT NULL DEFAULT '{}'       -- JSON: the broadcast id and, per session, delivered | queued | skipped_busy | skipped_not_limited
);
CREATE INDEX schedule_runs_ran ON schedule_runs(ran_at);
