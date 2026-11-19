-- Plan 4: reviews, the push outbox, and the tmux server of a session.
-- Differences from spec section 5 are listed in the plan (section 4.1).

CREATE TABLE reviews (
  n            INTEGER PRIMARY KEY,           -- allocation order
  id           TEXT NOT NULL UNIQUE,          -- 'R-' || n
  task_id      TEXT NOT NULL,
  session_id   TEXT NOT NULL DEFAULT '',      -- the session that called complete_task
  summary      TEXT NOT NULL,                 -- the agent's own completion summary
  tests_status TEXT NOT NULL CHECK (tests_status IN ('passed','failed','not_run')),
  tests_detail TEXT NOT NULL DEFAULT '',      -- the agent's one line about the tests
  status       TEXT NOT NULL CHECK (status IN ('pending','approved','changes_requested','merged','dropped')),
  approved_how TEXT NOT NULL DEFAULT '' CHECK (approved_how IN ('','merge','pr')),
  diff_stat    TEXT NOT NULL DEFAULT '',      -- git diff --stat as approved; '' until then
  head         TEXT NOT NULL DEFAULT '',      -- the branch's commit when approved
  pr_url       TEXT NOT NULL DEFAULT '',
  my_notes     TEXT NOT NULL DEFAULT '',      -- the note of a change request
  note_message TEXT NOT NULL DEFAULT '',      -- M-<n> that carries the note to the agent
  closed_via   TEXT NOT NULL DEFAULT '' CHECK (closed_via IN ('','popup','ssh','cli')),
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL,
  closed_at    TEXT
);
-- One review of a task waits at a time; a second complete_task updates it.
CREATE UNIQUE INDEX reviews_one_pending ON reviews(task_id) WHERE status = 'pending';
CREATE INDEX reviews_task ON reviews(task_id, n);

CREATE TABLE pushes (
  n          INTEGER PRIMARY KEY,
  kind       TEXT NOT NULL CHECK (kind IN ('decision','review')),
  ref        TEXT NOT NULL,                   -- D-<n> or R-<n>
  device     TEXT NOT NULL,                   -- a name from [ntfy.devices]
  status     TEXT NOT NULL CHECK (status IN ('pending','sending','sent','failed','dropped')),
  not_before TEXT NOT NULL,                   -- quiet hours move it to their end
  attempts   INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  token_hash TEXT NOT NULL DEFAULT '',        -- SHA-256 (hex) of the one-time action token; '' without buttons
  created_at TEXT NOT NULL,
  claimed_at TEXT,
  sent_at    TEXT,
  used_at    TEXT                             -- when its token answered the ticket
);
CREATE UNIQUE INDEX pushes_once ON pushes(kind, ref, device);
CREATE INDEX pushes_due ON pushes(not_before) WHERE status IN ('pending','sending');
CREATE UNIQUE INDEX pushes_token ON pushes(token_hash) WHERE token_hash != '';

-- Which tmux server a session's pane is on (#{socket_path}); NULL when unknown or outside tmux.
ALTER TABLE sessions ADD COLUMN tmux_socket TEXT;

-- Ruling 117: why an ended session ended (SessionEnd's reason, normalised to
-- [a-z0-9_]{1,64} or 'other'); NULL while the session is not ended, or unknown.
-- A tmux kill or crash ends Claude Code with 'other', a user's exit does not.
ALTER TABLE sessions ADD COLUMN end_reason TEXT;

-- Task 6 addition to the plan's DDL: the reads of one task's tickets
-- (store.TaskDecisions, and the expiry of its assumptions when its review
-- is approved) and of reviews by status (store.ListReviews) use an index,
-- not a scan of the whole table.
CREATE INDEX decisions_task ON decisions(task_id, n);
CREATE INDEX reviews_status ON reviews(status, n);
