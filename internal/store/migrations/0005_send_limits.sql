-- The send limits (Ruling 113): how many messages one agent sender made
-- lately, read by sender and time.
CREATE INDEX messages_agent_sends ON messages(sender_session, created_at) WHERE routed_by = 'send' AND sender = 'agent';
