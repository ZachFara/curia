-- 1 once the status line has given the session its context window and
-- percentage (Ingest); from then on upsert leaves context_used and
-- context_max alone, so the hook's transcript and table values never replace them.
ALTER TABLE sessions ADD COLUMN context_live INTEGER NOT NULL DEFAULT 0;
