-- Plan 2 fix wave (Ruling 86): an answer typed into a pane (delivered by
-- send_keys) is only typed, not confirmed, until the agent is seen to get
-- it: a UserPromptSubmit whose prompt names the decision, or the asking
-- session's own check_decision. confirmed_at is when that happened; NULL on
-- a send_keys delivery means typed and unconfirmed. Answers typed before
-- this migration count as confirmed when they were typed.
ALTER TABLE decisions ADD COLUMN confirmed_at TEXT;
UPDATE decisions SET confirmed_at = delivered_at WHERE delivered_via = 'send_keys';
