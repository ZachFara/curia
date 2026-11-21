-- F20: the directory a session's mode allows paths in, frozen when the mode is
-- set (the session's worktree, else its repository then; NULL: none) and
-- never changed by a hook event, so an agent that cds away does not move it.
ALTER TABLE sessions ADD COLUMN mode_root TEXT;
