-- F19: per-session CPU and memory of the pane's process tree, one row per
-- session per monitor tick; session_id NULL is reserved for the host or the
-- Docker VM (part 2). Pruned by age and capped (store.PruneResources).
CREATE TABLE resource_samples (
  n INTEGER PRIMARY KEY, at TEXT NOT NULL, session_id TEXT,
  cpu_pct REAL NOT NULL, rss_mb INTEGER NOT NULL
);
CREATE INDEX resource_samples_session ON resource_samples (session_id);
CREATE INDEX resource_samples_at ON resource_samples (at);
