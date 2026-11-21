-- The socket of the tmux server that holds the task's window pane (pane), so
-- a pane id is never taken for the task's on another server. Empty: not
-- recorded (a task spawned before this column), which means unknown.
ALTER TABLE tasks ADD COLUMN pane_socket TEXT NOT NULL DEFAULT '';
