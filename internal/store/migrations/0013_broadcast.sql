-- F18 broadcasts (Ruling 152): one id shared by the rows of one broadcast, one row per target session.
ALTER TABLE messages ADD COLUMN broadcast_id TEXT;
CREATE INDEX messages_broadcast ON messages(broadcast_id) WHERE broadcast_id IS NOT NULL;
