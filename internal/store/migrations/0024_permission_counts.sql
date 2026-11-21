-- Review C-3: one denial row per distinct (session, away period, tool, summary,
-- verdict) with a repeat count, so an agent cannot grow the table by repeating
-- itself nor hide a later refusal behind a flood. `at` stays the first time;
-- last_at is the latest; period is the open away period's start ('' none).
ALTER TABLE permission_events ADD COLUMN n INTEGER NOT NULL DEFAULT 1;
ALTER TABLE permission_events ADD COLUMN last_at TEXT;
ALTER TABLE permission_events ADD COLUMN period TEXT NOT NULL DEFAULT '';
-- Rows stored before this file collapse into their first row, which takes the count and the last time.
UPDATE permission_events SET
  n = (SELECT COUNT(*) FROM permission_events p WHERE p.session_id = permission_events.session_id AND p.tool = permission_events.tool AND p.summary = permission_events.summary AND p.verdict = permission_events.verdict),
  last_at = (SELECT MAX(p.at) FROM permission_events p WHERE p.session_id = permission_events.session_id AND p.tool = permission_events.tool AND p.summary = permission_events.summary AND p.verdict = permission_events.verdict)
WHERE id IN (SELECT MIN(id) FROM permission_events GROUP BY session_id, tool, summary, verdict);
DELETE FROM permission_events WHERE id NOT IN (SELECT MIN(id) FROM permission_events GROUP BY session_id, tool, summary, verdict);
CREATE UNIQUE INDEX permission_events_key ON permission_events (session_id, period, tool, summary, verdict);
