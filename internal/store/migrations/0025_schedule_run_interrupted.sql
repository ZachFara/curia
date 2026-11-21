-- F22: a run claimed but never finished (a crash after the claim) is marked
-- interrupted, so the user can see the message was not sent. SQLite cannot
-- change a CHECK, so the table is rebuilt with the new outcome.
CREATE TABLE schedule_runs_new (
  id          INTEGER PRIMARY KEY,
  schedule_id TEXT NOT NULL,
  due_at      TEXT NOT NULL,
  ran_at      TEXT NOT NULL,
  outcome     TEXT NOT NULL CHECK (outcome IN ('fired','skipped_late','skipped_no_target','monitor_down','interrupted')),
  results     TEXT NOT NULL DEFAULT '{}'
);
INSERT INTO schedule_runs_new SELECT id, schedule_id, due_at, ran_at, outcome, results FROM schedule_runs;
DROP TABLE schedule_runs;
ALTER TABLE schedule_runs_new RENAME TO schedule_runs;
CREATE INDEX schedule_runs_ran ON schedule_runs(ran_at);
