-- Spec section 5, with two differences: task_id is plain TEXT (the tasks
-- table arrives in Plan 3), and sessions gains transcript_offset.

-- A running or past agent session, tied to a tmux pane
CREATE TABLE sessions (
  id            TEXT PRIMARY KEY,   -- agent's own session id (Claude Code session_id)
  agent         TEXT NOT NULL,      -- claude | codex
  tmux_pane     TEXT,               -- e.g. %12, from $TMUX_PANE
  tmux_session  TEXT, tmux_window TEXT,
  cwd           TEXT, repo TEXT, branch TEXT, worktree TEXT,
  task_id       TEXT,
  state         TEXT NOT NULL,      -- working | waiting | needs_decision | idle | ended | stale
  model         TEXT,
  context_used  INTEGER, context_max INTEGER,
  cost_usd      REAL,
  subagents_active INTEGER DEFAULT 0,
  last_update   TEXT,               -- one-line summary of latest activity
  seen_at       TEXT,               -- last time I focused this pane
  transcript_path TEXT,
  started_at TEXT, updated_at TEXT,
  transcript_offset INTEGER NOT NULL DEFAULT 0  -- bytes of the transcript already read
);

-- Append-only raw hook events (debugging, replay, metrics)
CREATE TABLE events (
  id INTEGER PRIMARY KEY, session_id TEXT, kind TEXT, payload JSON, at TEXT
);
