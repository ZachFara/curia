-- F29 auto-spawn. ready_hash: hash of title and body when the user marked the task ready at a terminal
-- (NULL otherwise: a web move, an edit of status, a launch); auto_spawned_at: when the monitor started it.
ALTER TABLE tasks ADD COLUMN ready_hash TEXT;
ALTER TABLE tasks ADD COLUMN auto_spawned_at TEXT;
CREATE INDEX tasks_auto_spawned ON tasks(auto_spawned_at) WHERE auto_spawned_at IS NOT NULL;
-- The auto log. The spec reuses F22's schedule_runs (schedule_id = 'auto'); that table is not on this
-- branch, so the same rows live here until F22 lands.
CREATE TABLE auto_spawn_runs (
  id        INTEGER PRIMARY KEY,
  at        TEXT NOT NULL,
  outcome   TEXT NOT NULL CHECK (outcome IN ('fired','refused')),
  check_    TEXT NOT NULL DEFAULT '',   -- the refusing check (refused only)
  task_id   TEXT NOT NULL DEFAULT '',
  session_id TEXT NOT NULL DEFAULT '',
  detail    TEXT NOT NULL DEFAULT ''    -- cleaned text: why it was eligible, or why refused
);
CREATE INDEX auto_spawn_runs_at ON auto_spawn_runs(at);
