-- Plan 3: tasks, messages, and team membership on sessions.
-- Differences from spec section 5 are listed in the plan (section 4.1).

CREATE TABLE tasks (
  n              INTEGER PRIMARY KEY,          -- allocation order
  id             TEXT NOT NULL UNIQUE,         -- 'T-' || n
  title          TEXT NOT NULL,
  body           TEXT NOT NULL DEFAULT '',     -- the agent's starting brief
  repo           TEXT NOT NULL,                -- main repository top level
  base_branch    TEXT NOT NULL DEFAULT '',
  status         TEXT NOT NULL CHECK (status IN ('backlog','ready','dropped')),  -- only what the user sets
  source         TEXT NOT NULL DEFAULT 'local' CHECK (source IN ('local','backlog_md','linear')),
  source_ref     TEXT NOT NULL DEFAULT '',
  tags           TEXT NOT NULL DEFAULT '',     -- lowercase tags joined by one space
  branch         TEXT NOT NULL DEFAULT '',     -- curia/T-<n>-<slug>, set by the first spawn
  worktree       TEXT NOT NULL DEFAULT '',     -- path, set by spawn, cleared by clean
  team           INTEGER NOT NULL DEFAULT 0,   -- spawned with --team
  agent_model    TEXT NOT NULL DEFAULT '',     -- spawn's --model
  pane           TEXT NOT NULL DEFAULT '',     -- pane of the last launch
  resume_session TEXT NOT NULL DEFAULT '',     -- session id curia resume continues
  created_at     TEXT NOT NULL,
  updated_at     TEXT NOT NULL,
  launched_at    TEXT,                         -- the launch claim, then the launch
  paused_at      TEXT,
  cleaned_at     TEXT
);
-- An imported task is one row per source file.
CREATE UNIQUE INDEX tasks_source_ref ON tasks(repo, source, source_ref) WHERE source != 'local';

-- sessions.task_id and sessions.worktree exist since 0001 and are written from now on.
ALTER TABLE sessions ADD COLUMN team_id TEXT;      -- the team's name; NULL for a solo session
ALTER TABLE sessions ADD COLUMN team_role TEXT;    -- lead | teammate
ALTER TABLE sessions ADD COLUMN team_member TEXT;  -- the member's name in the team
CREATE INDEX sessions_task ON sessions(task_id) WHERE task_id IS NOT NULL AND task_id != '';

CREATE TABLE messages (
  n                INTEGER PRIMARY KEY,
  id               TEXT NOT NULL UNIQUE,       -- 'M-' || n
  text             TEXT NOT NULL,
  kind             TEXT NOT NULL CHECK (kind IN ('instruction','rule','task')),
  sender           TEXT NOT NULL DEFAULT 'user' CHECK (sender IN ('user','agent')),
  sender_session   TEXT NOT NULL DEFAULT '',
  target_session   TEXT NOT NULL DEFAULT '',
  target_task      TEXT NOT NULL DEFAULT '',
  target_repo      TEXT NOT NULL DEFAULT '',   -- '' with kind rule means global
  suggested_target TEXT NOT NULL DEFAULT '',   -- the router's pick, as session:<id>, task:<id>, repo:<path> or global
  suggested_kind   TEXT NOT NULL DEFAULT '',
  runner_up        TEXT NOT NULL DEFAULT '',
  confidence       REAL,                       -- NULL when no model was asked
  routed_by        TEXT NOT NULL CHECK (routed_by IN ('address','confirmed','overridden','auto','manual','send')),
  auto_eligible    INTEGER NOT NULL DEFAULT 0, -- passed every auto-send guard but sampling
  status           TEXT NOT NULL CHECK (status IN ('held','queued','delivered','undone','canceled')),
  delivered_via    TEXT NOT NULL DEFAULT '' CHECK (delivered_via IN ('','send_keys','stop_hook','check','applied')),
  result_ref       TEXT NOT NULL DEFAULT '',   -- D-<n> of the rule or T-<n> of the task it became
  created_at       TEXT NOT NULL,
  hold_until       TEXT,
  delivered_at     TEXT,
  confirmed_at     TEXT                        -- a typed line the agent is known to have received
);
-- Instructions an agent has not got yet: queued, or typed and not confirmed.
CREATE INDEX messages_undelivered ON messages(target_session)
  WHERE status = 'queued' OR (status = 'delivered' AND delivered_via = 'send_keys' AND confirmed_at IS NULL);
-- The accuracy gate reads the newest labels.
CREATE INDEX messages_labels ON messages(n) WHERE routed_by IN ('confirmed','overridden');
