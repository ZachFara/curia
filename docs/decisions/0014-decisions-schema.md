# 0014. The decisions schema

Status: accepted (Plan 2 setup)

## Context

Spec section 5 sketches a `decisions` table with a text `id` and an FTS5 index for precedent search. FTS5 with external content needs a stable integer rowid, several processes create tickets at once, and Plan 2's delivery, withdrawal and supersede rules need columns the sketch does not have. The exact DDL is in plan section 4.1 and lands as `internal/store/migrations/0002_decisions.sql` (Task 9).

## Decision

- **Differences from spec section 5:** `n INTEGER PRIMARY KEY` is added and `id` becomes `UNIQUE` (an implicit rowid can change on `VACUUM`); text columns are `NOT NULL DEFAULT ''`; `answer_key`, `differs_from`, `differs_why`, `withdraw_reason`, `superseded_by`, `superseded_at`, `delivery`, `delivered_via`, `delivered_at`, `expires_at` and `closed_at` are added; `answered_via` gains `cli`. The migration also adds a `meta` table (the notification rate limit) and `sessions.cache_expires_at` and `sessions.cache_rebuild_usd` (F17; written in Plan 5).
- **Ids** are `D-<n>`, decimal, from `D-1`, allocated in `CreateDecision`'s `BEGIN IMMEDIATE` transaction as `'D-' || (SELECT COALESCE(MAX(n), 0) + 1 FROM decisions)`. Rows are never deleted, so ids are never reused. An id from outside is accepted only if it matches `^D-[1-9][0-9]{0,8}$`.
- **Duplicates:** a partial unique index on `(session_id, args_hash) WHERE status = 'open'` stops an identical retry from opening a second ticket. Because a past-deadline `assume` twin can still read `open` before any sweep, `CreateDecision` first expires that session's due twin inside its own transaction (verified: without it the insert fails with `UNIQUE constraint failed`).
- **Full-text search:** `decisions_fts` is an FTS5 external-content table over `question`, `context`, `rationale`, `answer`, `answer_reasoning`, `tags`, with `tokenize='porter unicode61'`, kept in sync by three triggers. No Go code writes to it. A `MATCH` argument is only ever built by `precedent.MatchExpr`.
- **Expiry** is a stored deadline (`expires_at`) checked by every "open" query; the status flip is housekeeping.

## Consequences

- Verified while writing the plan, in a throwaway probe against the pinned `modernc.org/sqlite` v1.62.0 (SQLite 3.54.1, `ENABLE_FTS5` compiled in): the DDL runs as one multi-statement `Exec` inside one transaction, as `store.migrate` does; 200 concurrent inserts through 5 handles produced `D-1` to `D-200` with no duplicate; the partial unique index rejected a duplicate open ticket; porter matched `migration` against `migrations`; `bm25` with column weights and `UPDATE ... RETURNING` work; FTS5 `integrity-check` passed after updates and failed once a trigger was dropped.
- `TestFTSStaysInSync` (Task 9) keeps the triggers honest; `store.upsert` names its columns, so the two new `sessions` columns survive every session write.
- One more column than the spec, and porter stemming is English-only. Whether `docs/DESIGN.md` should be updated to match is plan section 14, open question 4.

Amended 2026-11-12: Ruling 64 (PROGRESS.md) changes the duplicate rule above: a superseded decision is not a duplicate, so an identical retry after supersession opens a new ticket. Ruling 65: a non-empty `SupersededBy` implies supersede in the store, and a repeated supersede keeps the first `superseded_at`.
