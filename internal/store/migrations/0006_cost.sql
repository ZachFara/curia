-- Plan 5: usage and cost samples, task priority and agent, the export mark.
-- Differences from spec F16 are listed in the plan (section 4.1).

CREATE TABLE usage_samples (
  n          INTEGER PRIMARY KEY,
  at         TEXT NOT NULL,                  -- when the reading was taken
  window     TEXT NOT NULL CHECK (window IN ('five_hour','seven_day')),
  used_pct   REAL NOT NULL CHECK (used_pct >= 0 AND used_pct <= 100),
  resets_at  TEXT,                           -- NULL when the reading had none
  session_id TEXT NOT NULL DEFAULT ''        -- whose ingest carried it; the windows are account-wide
);
CREATE INDEX usage_samples_window ON usage_samples(window, at);

CREATE TABLE cost_samples (
  n          INTEGER PRIMARY KEY,
  at         TEXT NOT NULL,
  session_id TEXT NOT NULL,
  cost_usd   REAL NOT NULL CHECK (cost_usd >= 0)   -- the session's running total
);
CREATE INDEX cost_samples_session ON cost_samples(session_id, at);

ALTER TABLE tasks ADD COLUMN priority INTEGER NOT NULL DEFAULT 2 CHECK (priority IN (1,2,3));
ALTER TABLE tasks ADD COLUMN agent TEXT NOT NULL DEFAULT 'claude' CHECK (agent IN ('claude','codex'));
ALTER TABLE decisions ADD COLUMN exported_at TEXT;  -- when its Obsidian note was last written

-- Ruling P5-R2, an addition to the plan's DDL: the status line reports how
-- many tokens a rebuild of the cold cache would write, not dollars, and
-- Curia keeps no price table. Written only by store.Ingest; NULL when unknown.
ALTER TABLE sessions ADD COLUMN recache_tokens INTEGER;
