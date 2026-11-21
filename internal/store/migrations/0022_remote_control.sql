-- F21 Remote Control: what Curia believes about a session's Remote Control
-- (off | pending | on | unknown), when it last looked, and how it knows
-- (indicator, typed, typing, unconfirmed). NULL reads as off.
ALTER TABLE sessions ADD COLUMN rc TEXT;
ALTER TABLE sessions ADD COLUMN rc_how TEXT;
ALTER TABLE sessions ADD COLUMN rc_checked_at TEXT;
