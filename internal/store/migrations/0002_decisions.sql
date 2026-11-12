-- Plan 2: decision tickets, their full-text index, and two small additions.
-- Differences from spec section 5 are listed in the plan (section 4.1).

CREATE TABLE decisions (
  n              INTEGER PRIMARY KEY,          -- allocation order; rowid of decisions_fts
  id             TEXT NOT NULL UNIQUE,         -- 'D-' || n
  session_id     TEXT NOT NULL DEFAULT '',     -- asking session; '' when the caller is unknown
  task_id        TEXT NOT NULL DEFAULT '',     -- always '' until Plan 3
  repo           TEXT NOT NULL DEFAULT '',     -- git top-level path of the asking session
  question       TEXT NOT NULL,
  context        TEXT NOT NULL DEFAULT '',
  options        TEXT NOT NULL,                -- JSON [{"key","label","tradeoffs"}]
  recommendation TEXT NOT NULL,                -- option key
  rationale      TEXT NOT NULL,
  reversibility  TEXT NOT NULL CHECK (reversibility IN ('cheap','costly','irreversible')),
  mode           TEXT NOT NULL CHECK (mode IN ('blocking','assume')),
  assumption     TEXT NOT NULL DEFAULT '',
  scope          TEXT NOT NULL CHECK (scope IN ('task','repo','global')),
  tags           TEXT NOT NULL DEFAULT '',     -- lowercase tags joined by one space
  status         TEXT NOT NULL CHECK (status IN ('open','answered','confirmed','overridden','withdrawn','expired')),
  answer_key     TEXT NOT NULL DEFAULT '',     -- chosen option key; '' for a free-text answer
  answer         TEXT NOT NULL DEFAULT '',     -- '<key>: <label>' or the free text (indexed)
  answer_reasoning TEXT NOT NULL DEFAULT '',
  answered_via   TEXT NOT NULL DEFAULT '' CHECK (answered_via IN ('','popup','ssh','cli','ntfy')),
  precedent_of   TEXT NOT NULL DEFAULT '',     -- decision id cited when withdrawing
  differs_from   TEXT NOT NULL DEFAULT '',     -- precedent the agent says does not apply
  differs_why    TEXT NOT NULL DEFAULT '',
  withdraw_reason TEXT NOT NULL DEFAULT '',
  superseded_by  TEXT NOT NULL DEFAULT '',
  args_hash      TEXT NOT NULL,
  delivery       TEXT NOT NULL DEFAULT 'none' CHECK (delivery IN ('none','queued','delivered')),
  delivered_via  TEXT NOT NULL DEFAULT '' CHECK (delivered_via IN ('','send_keys','stop_hook','check')),
  created_at     TEXT NOT NULL,
  expires_at     TEXT,                         -- assume tickets only: created_at + assume_expiry
  answered_at    TEXT,                         -- answered, confirmed, overridden
  closed_at      TEXT,                         -- withdrawn, expired
  superseded_at  TEXT,
  delivered_at   TEXT
);

-- An identical retry from the same session cannot open a second ticket.
CREATE UNIQUE INDEX decisions_open_dedupe ON decisions(session_id, args_hash) WHERE status = 'open';
CREATE INDEX decisions_session_status ON decisions(session_id, status);
CREATE INDEX decisions_status_created ON decisions(status, created_at);

CREATE VIRTUAL TABLE decisions_fts USING fts5(
  question, context, rationale, answer, answer_reasoning, tags,
  content='decisions', content_rowid='n', tokenize='porter unicode61');

CREATE TRIGGER decisions_ai AFTER INSERT ON decisions BEGIN
  INSERT INTO decisions_fts(rowid, question, context, rationale, answer, answer_reasoning, tags)
  VALUES (new.n, new.question, new.context, new.rationale, new.answer, new.answer_reasoning, new.tags);
END;
CREATE TRIGGER decisions_ad AFTER DELETE ON decisions BEGIN
  INSERT INTO decisions_fts(decisions_fts, rowid, question, context, rationale, answer, answer_reasoning, tags)
  VALUES ('delete', old.n, old.question, old.context, old.rationale, old.answer, old.answer_reasoning, old.tags);
END;
CREATE TRIGGER decisions_au AFTER UPDATE OF question, context, rationale, answer, answer_reasoning, tags ON decisions BEGIN
  INSERT INTO decisions_fts(decisions_fts, rowid, question, context, rationale, answer, answer_reasoning, tags)
  VALUES ('delete', old.n, old.question, old.context, old.rationale, old.answer, old.answer_reasoning, old.tags);
  INSERT INTO decisions_fts(rowid, question, context, rationale, answer, answer_reasoning, tags)
  VALUES (new.n, new.question, new.context, new.rationale, new.answer, new.answer_reasoning, new.tags);
END;

-- Small cross-process facts (the notification rate limit).
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);

-- F17: written by `curia ingest` in Plan 5; read by the ticket queue now.
ALTER TABLE sessions ADD COLUMN cache_expires_at TEXT;
ALTER TABLE sessions ADD COLUMN cache_rebuild_usd REAL;
