-- Review C2: the timeline's WebAction source (events) and its away-period
-- sources read an index, never the whole table. Additive.
CREATE INDEX events_web_at ON events (at) WHERE kind = 'WebAction';
CREATE INDEX away_runs_started ON away_runs (started_at);
CREATE INDEX away_runs_ended ON away_runs (ended_at);
