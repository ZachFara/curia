# 0042. Refusals under away are counted, never dropped

Status: accepted (2026-11-21, Rulings 230 and 231; final review C-3; PR #209, migration `0024_permission_counts.sql`).

## Context

While the user is away the hook denies what nobody could approve and stores each denial so `curia back` can list them. One row per denial let an agent that loops on a denied call grow the table without limit (1,000,000 rows made the summary query take 0.78 s), and a cap that silently drops rows would hide what the user most needs to see.

## Decision

- A denial is one row per session, away period, tool and summary, with a repeat count, written by one upsert.
- At most 500 distinct rows per session per period (`PermissionDistinctCap`); past that one overflow row counts the rest, and the summary says "N further refusal(s) not recorded". A refusal is counted somewhere, never dropped.
- `curia back` lists them in the order first seen (a repeat cannot outrank something seen once), the first few, and `curia back --all` lists every one of the last period, even when the summary was already shown.
- Denial rows older than 30 days are pruned by the monitor's once-a-minute prune, with the run tables (90 days) and old config revisions (see decision 0038).

## Consequences

- The 500 and "the first few" are choices of the build, not of the spec.
- An away period longer than 30 days loses its oldest refusals.
