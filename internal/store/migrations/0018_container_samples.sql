-- F19 part 2: the Docker containers the monitor last saw (replaced whole each
-- tick) and the Docker VM's memory, kept on a resource_samples row with a NULL
-- session_id.
ALTER TABLE resource_samples ADD COLUMN docker_mem_mb INTEGER;
ALTER TABLE resource_samples ADD COLUMN docker_mem_limit_mb INTEGER;
CREATE TABLE containers (
  id TEXT PRIMARY KEY, session_id TEXT, name TEXT NOT NULL,
  attributed_by TEXT NOT NULL,   -- label | compose | none
  cpu_pct REAL NOT NULL, mem_mb INTEGER NOT NULL, seen_at TEXT NOT NULL
);
