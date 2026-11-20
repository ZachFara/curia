-- F28: every change of the user config file, whole (actor: cli, dashboard,
-- rollback, external). Rows are only ever appended.
CREATE TABLE config_revisions (
  n INTEGER PRIMARY KEY, at TEXT NOT NULL, actor TEXT NOT NULL,
  key TEXT NOT NULL, old TEXT NOT NULL, new TEXT NOT NULL, file TEXT NOT NULL
);
