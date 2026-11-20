-- F26: web device tokens. Only the SHA-256 of a token is stored; revoking
-- deletes the row.
CREATE TABLE web_devices (
  name TEXT PRIMARY KEY, token_sha256 TEXT NOT NULL UNIQUE, created_at TEXT NOT NULL
);
