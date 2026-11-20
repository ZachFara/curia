-- F5 amendment: tasks filed by an agent (file_task). filed_by is NULL for a task the user wrote.
ALTER TABLE tasks ADD COLUMN filed_by TEXT;         -- the filing session id
ALTER TABLE tasks ADD COLUMN filed_reason TEXT;     -- the agent's reason
ALTER TABLE tasks ADD COLUMN filer_lineage TEXT;    -- JSON: team, role, lead as Curia saw them; display and audit only
CREATE INDEX tasks_filed ON tasks(filed_by, created_at) WHERE filed_by IS NOT NULL;
