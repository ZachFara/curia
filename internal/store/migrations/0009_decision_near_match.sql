-- F25 (Ruling 160): the decision request_decision returned as a near-duplicate
-- when this ticket was raised; "asked again" counts these. NULL: none.
ALTER TABLE decisions ADD COLUMN near_match TEXT;
CREATE INDEX decisions_near_match ON decisions(near_match) WHERE near_match IS NOT NULL;
-- Reuse counts the links of precedent_of (set on a withdrawal).
CREATE INDEX decisions_precedent_of ON decisions(precedent_of) WHERE precedent_of != '';

-- The timeline is a union of bounded reads of the time columns, newest first.
CREATE INDEX events_stop_at ON events(at) WHERE kind = 'Stop';
CREATE INDEX sessions_started ON sessions(started_at);
CREATE INDEX sessions_ended ON sessions(updated_at) WHERE state = 'ended';
CREATE INDEX decisions_created ON decisions(created_at);
CREATE INDEX decisions_answered ON decisions(answered_at) WHERE answered_at IS NOT NULL;
CREATE INDEX decisions_closed ON decisions(closed_at) WHERE closed_at IS NOT NULL;
CREATE INDEX reviews_created ON reviews(created_at);
CREATE INDEX reviews_closed ON reviews(closed_at) WHERE closed_at IS NOT NULL;
CREATE INDEX messages_delivered ON messages(delivered_at) WHERE delivered_at IS NOT NULL;
CREATE INDEX tasks_created ON tasks(created_at);
CREATE INDEX pushes_sent ON pushes(sent_at) WHERE sent_at IS NOT NULL;
