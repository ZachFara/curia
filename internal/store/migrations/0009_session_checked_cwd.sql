-- The directory the hook last asked git about for a session with no
-- repository, so it asks again only when the cwd changes. NULL: not asked.
ALTER TABLE sessions ADD COLUMN checked_cwd TEXT;
