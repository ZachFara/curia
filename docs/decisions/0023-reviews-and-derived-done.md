# 0023. Reviews and a derived `done`

Status: accepted (Plan 4 setup)

## Context

Spec F10 wants finished work to wait in one queue with everything needed to judge it, and merged by one key. Plan 3's rule (decision 0016) is that a task's shown status is derived on every read so the board cannot go stale, and `tasks.status` has a `CHECK` that allows only `backlog`, `ready` and `dropped`. Spec section 5 sketches a `reviews` table. The MCP server never runs git (Ruling 70), and only the owner decides, approves or merges (spec section 7).

## Decision

- **A review is a row in `reviews`, opened only by the agent's `complete_task` MCP tool and closed only by `curia review`.** The exact DDL is in plan section 4.1 and lands as `internal/store/migrations/0005_reviews.sql` (Plan 4 Task 6; the number follows Plan 3's `0004`). Differences from the spec: `n` and a `UNIQUE` id (`R-<n>`), `tests_status` as an enum (`passed`, `failed`, `not_run`) with `tests_detail` beside it, `status` gains `dropped`, and `approved_how`, `head`, `pr_url`, `note_message`, `closed_via` and `updated_at` are added. One review of a task is pending at a time (`reviews_one_pending`); a second `complete_task` updates it, and after a closed review it opens a new one.
- **`review` and `done` are derived from the task's newest review; `tasks` is not altered.** `task.Derive` gains two rows above `needs_decision`: stored `dropped` first, then a newest review that is `pending` shows `review`, then one that is `approved` or `merged` shows `done`. A newest review that is `changes_requested` or `dropped` changes nothing. The cost if wrong: a stored `done` is one migration later.
- **`complete_task` is the sixth tool and only says "ready for review".** It takes `summary`, `tests_status` and an optional `tests_detail`. A teammate gets `ask_lead` (only the lead speaks for the job), and a session with no task gets `no_task`. It runs no git and reads no file; the diff is computed when the owner looks and recorded when the owner approves (`reviews.diff_stat` is empty until then). What it stores is the agent's claim and is shown as such.
- **Approve defaults to a local `git merge --no-ff --no-edit` in the owner's checkout, and only when the base is checked out there;** `--pr` (or `review.approve = "pr"`) pushes the branch and runs `gh pr create`. The merge uses git's own message and runs the repository's hooks. Any failure runs `git merge --abort` when `MERGE_HEAD` exists, and the review stays pending. Curia never passes `--force`, never pushes the base branch and never runs `git reset` or `git checkout` in the owner's checkout.
- **Approval expires the task's open assume tickets in both modes and leaves blocking tickets open** (F8: "or when its task merges"). In pull-request mode the review ends `approved` and nothing later flips it to `merged`.
- **A change request is a Plan 3 message** addressed to the task's live lead, else queued for the review's session, so it travels under every typing-path guard. **Dropping a review drops the task and does not stop its agent.**

## Consequences

- `curia review`, `curia restore` and `curia serve` refuse to run when `CLAUDECODE` is set; the guard is not a boundary, as before. `curia reviews ls` only reads.
- An older `curia mcp` keeps five tools; an older upsert leaves the new column alone (`TestOldUpsertKeepsSocket`).
- Plan sections 4.1, 4.3, 4.4, 4.6 and decisions 1 to 8 of section 13 carry the detail and the reasons.
