# Curia Implementation Plan 4: Review Queue, Remote Answers and Restore

Nov 13, 2026 · @Zachariah Farahany

## TLDR

Plan 4 closes the loop from a running agent to merged work, and lets me leave the desk. It adds the review queue (F10): an agent calls `complete_task`, the work waits in `curia review` with its summary, diff, tests, decisions and unconfirmed assumptions, and I approve (merge or pull request), ask for changes, or drop it. It adds ntfy pushes with quiet hours and an optional `curia serve` endpoint that answers a blocking ticket from a phone button (F11), and `curia restore`, which brings the agents back after a reboot or a tmux crash (F12). It ships in 22 test-first tasks across 6 waves on the integration branch `plan-4`, after Plan 3 has merged, with five probes run first. It is done when the built binary takes a spawned task through `complete_task`, a change request, a second completion and a merge, pushes a blocking ticket to a stand-in ntfy server and answers it through `curia serve`, and restores the agent into a new tmux server.

## Contents

- Plan 3 dependencies to re-verify
1. Plan sequence
2. Global constraints
3. Review focus
4. Decided here: schema, completion, status, review actions, pushes, serve, server identity, restore, views, config, inputs
5. File structure
6. Setup (orchestrator, before Wave 1)
7. Waves
8. Tasks 1 to 4 (Wave 1)
9. Tasks 5 to 8 (Wave 2)
10. Tasks 9 to 13 (Wave 3)
11. Tasks 14 to 18 (Wave 4)
12. Tasks 19 to 22 (Waves 5 and 6)
13. Decisions made in this plan
14. Live probes before Wave 1
15. Questions only the owner can answer
16. Out of scope
- Appendix A: the interface and file check

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. An implementer reads `AGENTS.md`, section 2, its own task, and the parts of section 4 named on that task's **Brief includes** line, pasted into the brief verbatim. There is no per-task reviewer and no pre-flight review (owner's ruling): the only review is the final PR, so a task's named tests are its gate and every one of them must exist and pass.

**Goal:** finished work waits in one queue with everything needed to judge it and is merged by one key; a blocking ticket reaches my phone and can be answered from it; a reboot costs minutes, not context.

**Architecture:** a review is a row in a new `reviews` table, opened only by the agent's `complete_task` MCP tool and closed only by `curia review`, which runs git (and `gh`) itself. A task's `review` and `done` statuses are derived from its newest review, like every other derived status. A change request travels to the agent as a Plan 3 message, so it is typed or handed over under the same guards as everything else. Pushes are rows in an outbox table, sent over plain `net/http` to ntfy by whichever Curia process is alive (each agent's `curia mcp`, or `curia serve`), which is also what releases a push held back by quiet hours; there is still no daemon. A phone button carries a one-time token bound to one ticket, and `curia serve` answers through the same `deliver.Answer` path as `curia decide`. `curia restore` reads the store, opens one tmux window per lost session with Plan 3's `NewWindow`, and runs `claude --resume` in it.

**Tech stack:** unchanged (Go 1.26, `modernc.org/sqlite` v1.62.0, cobra, Bubble Tea v1, lipgloss, BurntSushi/toml). No new module: ntfy and the endpoint use `net/http` from the standard library.

**Spec:** `docs/DESIGN.md`. This plan implements F10, F11, F12, the `reviews` table of section 5, the `R` count of the badge, the review-ready rank and the `r` key that F3 and F4 left for the review queue, F8's "review link" and "expires when its task merges", and the rules of section 7 for every new input.

**Code as built:** `main` at 33db0e4 (Plans 1 and 2, release packaging, and the installer fix of Rulings 94 to 96 merged; Plan 3 is being built on `plan-3`, where Tasks 1, 2, 3 and 9 have merged). Every interface this plan takes from Plans 1 and 2 was checked against that code. Every interface it takes from Plan 3 is quoted from the Plan 3 plan in the next section and must be checked again against the code once Plan 3 has merged.

## Plan 3 dependencies to re-verify

Plan 4 starts only after `plan-3` has merged into `main`. Before section 6's setup PR the orchestrator checks each line below against the merged code. A difference in a name or signature is corrected in the tasks named, by amending this document; a difference in behaviour is a Ruling.

| # | Plan 3 item, as its plan states it | Used by tasks |
| --- | --- | --- |
| D1 | Migration `0004_tasks.sql` is the newest migration. `tasks` has `id`, `title`, `body`, `repo`, `base_branch`, `status CHECK (status IN ('backlog','ready','dropped'))`, `branch`, `worktree`, `team`, `agent_model`, `pane`, `resume_session`, `launched_at`, `paused_at`. `messages.routed_by` allows `'address'`; `messages.status` allows `'queued'`. If a Plan 3 fix wave added `0005`, this plan's migration takes the next free number and every "5 migrations" below becomes that count. | 6, 9 |
| D2 | `model.Task` with `ID, Title, Body, Repo, BaseBranch, Branch, Worktree, AgentModel, Pane, ResumeSession string`, `Team bool`, `Status model.TaskStatus`, `LaunchedAt, PausedAt time.Time`; constants `model.TaskReview "review"`, `model.TaskDone "done"`, `model.TaskDropped`. `model.Session` has `TaskID, Worktree, TeamID, TeamRole, TeamMember string`; `model.RoleLead = "lead"`, `model.RoleTeammate = "teammate"`. | 1, 4, 5, 11, 16, 18 |
| D3 | `model.Message` with `Text`, `Kind`, `Sender`, `TargetSession`, `TargetTask`, `TargetRepo`, `RoutedBy`, `Status`, `CreatedAt`; `model.KindInstruction`, `model.SenderUser`, `model.RoutedAddress`, `model.MsgQueued`; `type MessageOutcome struct { Message Message; Note string }`. | 16, 18 |
| D4 | `func (s *Store) GetTask(ctx context.Context, id string) (model.Task, bool, error)`; `func (s *Store) ListTasks(ctx context.Context, f TaskFilter) ([]model.Task, error)`; `func (s *Store) UpdateTask(ctx context.Context, id string, fn func(model.Task) (model.Task, error)) (model.Task, error)`; `func (s *Store) TaskSessions(ctx context.Context, taskID string) ([]model.Session, error)` (live only, most recently updated first); `func (s *Store) OpenBlockingByTask(ctx context.Context, now time.Time) (map[string]int, error)`; `store.ErrNoTask`. | 6, 11, 16, 18 |
| D5 | `func (s *Store) CreateMessage(ctx context.Context, m model.Message) (model.Message, error)`; `func (s *Store) BadgeExtras(ctx context.Context) (undeliveredMessages int, autoOff bool, err error)`. `store.upsert` names its columns and writes `task_id` and `worktree`, never the team columns. | 6, 16, 18, 21 |
| D6 | `func Message(ctx context.Context, st *store.Store, tm tmux.Client, log *slog.Logger, id string, now time.Time) (model.MessageOutcome, error)` in `internal/deliver/message.go`, sharing the pane check with `deliver.Answer`. | 14, 16, 18 |
| D7 | `message.MaxText = 800`; a typed message reads `Message M-12 from the user: …`. | 5, 16, 22 |
| D8 | `task.ValidID(id string) bool`, `task.ValidModel(m string) bool`, `task.ValidBranch(b string) bool`, `task.SessionName(repo string) string`, `type Problem struct{ Field, Problem string }`, `func Derive(t model.Task, sessions []model.Session, openBlocking int) model.TaskStatus`, `type Card struct { Task model.Task; Status model.TaskStatus; Sessions []model.Session; Pane string }`, `func Cards(tasks []model.Task, sessions []model.Session, openBlocking map[string]int) []Card`, `func Brief(t model.Task, team bool) string` whose last rule is the line quoted in Task 5. | 5, 11, 16, 18 |
| D9 | `tmux.WindowSpec { Session, Name, Dir string; Env, Command []string }` and `NewWindow(ctx context.Context, w WindowSpec) (Pane, error)` on `tmux.Client`, with the refusals of Plan 3 Task 3 (names `^[A-Za-z0-9_-]{1,40}$`, each `Env` `^[A-Z_][A-Z0-9_]{0,63}=[A-Za-z0-9._/:@-]{0,256}$`). | 18 |
| D10 | `curia launch <id> [--model <m>] [--team] [--resume <uuid>]` (hidden), which replaces itself with `claude … --resume <uuid>` in an environment holding `CURIA_TASK_ID`. `executablePath()`. | 18 |
| D11 | In `cmd/curia`: `taskCards(ctx context.Context, st *store.Store, now time.Time) ([]task.Card, error)` (`task.go`); `worktreeDirty(ctx context.Context, path string) (bool, error)` and `refExists(ctx context.Context, repo, ref string) (bool, error)` (`worktree.go`); `sendOutcomeLine(o model.MessageOutcome) string` (`send.go`); `reconcile` ending with `refreshTeams`. | 11, 16, 18, 21 |
| D12 | `mcp.Caller` has `TaskID` filled and `TeamRole`; `mcp.Tools` returns five tools, the fifth `check_messages`; a teammate's `request_decision` is refused with code `ask_lead`. `install.toolRules` holds five names. `e2e/testdata/decisions/tools_list.golden.json` lists five. | 3, 12 |
| D13 | `ui.BoardSource`, `ui.NewBoard(src BoardSource, tm tmux.Client, now func() time.Time, embedded bool) BoardModel`, the board in `internal/ui/board.go` and `board_view.go` with four columns; `ui.Model.WithBoard`; the dashboard's `TASK` column and team group rows; `ui.Source.Tasks`. | 4, 20 |
| D14 | `hook.Env` has `TaskID string` and `Git func(cwd string) (repo, branch, worktree string)`; `PaneInfo func(pane string) (tmuxSession, window string)` is unchanged from Plan 2. | 13 |
| D15 | `fleet.Counts` has `Decisions, Undelivered, Waiting, Reviews int` and `AutoOff bool`; `fleet.Count(s []model.Session, openBlocking, undelivered int) Counts`; `fleet.Units` ranks with the package's `group`. | 10, 21 |
| D16 | `testdata/tasks/claude-stub.sh` (the stand-in `claude`, which handles `--resume <id>` and draws the empty prompt box) and `e2e/tasks_test.go`'s helpers; `e2e/testdata/tasks/stub_argv.golden.txt` holds the brief byte for byte. | 5, 22 |
| D17 | Plan 3's probe results: P3 (hooks see the agent's environment), P4 (`claude --resume <id>` keeps the session id), P5 (the Agent Teams switches). Section 14 builds on them. | 18 |
| D18 | The installer as merged in `main` (Rulings 94 to 96, checked against the code at 33db0e4): `internal/install/tmuxconf.go` keeps the marker in the option `@curia_marker_format` (`const markerFormat = "#{?#{==:#{@curia_marker},red},#[fg=red] ●,}"`), appends `#{E:@curia_marker_format}` to the window formats in effect, and binds a key only when it is free or already Curia's (`key_dashboard`, `key_decisions`, `key_next`). Plan 4 changes the value of `markerFormat` and nothing else there; it adds no key and no option. | 3, 10 |
| D19 | Plan 3's rulings so far: 97 (a message's stored status is `canceled`, Go name `model.MsgCanceled`; never write "cancelled" in an identifier or a stored value), 98 (the real team config: a teammate is identified only by its pane, and the team directory is deleted when the lead exits), 100 (a long Stop-hook text may be refused by the model; short plain text is followed), 101 (the idle footer may carry `· /tasks to see subagents`), 102, 103. Plan 3's Task 3 as built: a one-argument window command must be a plain word, and the caller must check that `Dir` exists. | 16, 18 |

## 1. Plan sequence

Row 4 of Plan 1's table: "finished work is reviewed and merged from one queue; a ticket reaches the phone; a restart is recoverable". Plan 5 (F13, F15, F16) follows. What Plan 4 leaves for it is in section 16.

## 2. Global constraints

Every task's requirements include these, and every constraint of Plans 1 to 3 still holds unless amended here.

- **Hook budget and output.** Unchanged: under 20 ms per event; nothing on stdout or stderr and exit 0 on every path, except the one `{"decision":"block","reason":"…"}` line of a `Stop` that claimed something. Plan 4 adds to the hook path exactly two things. On a session's first event, the tmux call that already runs (`PaneInfo`) returns one more field, the server's socket path, and the session upsert writes one more column: no process, statement or file read is added. And on the few events that change a window's marker, `attention.Apply`, which already costs a tmux call of about 8 ms, runs one more indexed read (`store.PendingReviewTasks`); an event that changes no marker runs nothing new. Nothing in a hook reads `pushes` or touches the network. Measured by the existing in-process and whole-process latency tests, unchanged (Task 13).
- **Only I decide, approve or merge (spec section 7).** No MCP tool approves a review, requests changes, drops a task or answers a ticket. The MCP server exposes exactly six tools: Plan 3's five and `complete_task`, which only says "this task is ready for review". `curia review`, `curia restore` and `curia serve` refuse to run when `CLAUDECODE` is set and not empty (`refuseInsideAgent`, a guard and not a boundary, as before). `curia reviews ls` only reads and is allowed anywhere.
- **Everything from outside is data.** The agent's summary and test line, my review note, git and `gh` output, branch names, an HTTP request, a stored session row read back by `restore`, and the ntfy server's reply are untrusted. Each is bounded and cleaned by `internal/sanitize` when it enters the store and again wherever it is printed, typed or sent; section 4.13 lists every new input. None is executed, interpolated into a script, passed to a shell, or built into SQL.
- **Typing into panes.** Unchanged. Plan 4 types nothing new by its own code: a change request and the restore notice to a team lead are Plan 3 messages delivered by `deliver.Message`, and a phone answer goes through `deliver.Answer`, so the whole order of conditions in `AGENTS.md` ("The typing path, final order of conditions", Rulings 81 to 93) applies to them as it stands. Task 14 adds one more refusal to that order and removes none.
- **git and gh.** Every git and `gh` call is an argv run without a shell, through `runGit` (which drops git's repository variables) or a seam; a value that came from a task (`base`, `branch`, a path) is checked by `task.ValidBranch` or built by Curia, and is never the first argument after the subcommand's fixed flags. Curia never passes `--force` to `merge`, `push`, `worktree remove` or `branch`, never pushes the base branch, and never runs `git reset` or `git checkout` in my checkout.
- **Network is opt-in (spec section 7).** With no `[ntfy] url` configured no Curia process opens a connection, and `curia serve` is a command I start. It binds only to a loopback or Tailscale address. No test touches the real network: ntfy is a local `httptest` server.
- **Secrets.** The ntfy access token is read from the environment variable `CURIA_NTFY_TOKEN` and never from a file. A one-time action token exists in clear only in the outgoing request; the store keeps its SHA-256. No token is ever logged or printed.
- **tmux installer rulings (2026-11-13), as built in `internal/install/tmuxconf.go`.** Ruling 94: the snippet never replaces `window-status-format`. Ruling 95: Curia never rebinds a key the user has bound. Ruling 96: tmux options are changed only as far as needed. Plan 4 changes one thing in the snippet: the value of Curia's own option `@curia_marker_format` (Task 10). It adds no line that touches a format, no key and no other option; a task that finds it needs one stops and reports.
- **One commit per task.** Implementers share one `make check` lock across worktrees. Before the single gated commit an implementer runs, on its own packages only, `gofumpt -l`, `.bin/golangci-lint run ./<its packages>/...`, `go vet` and the focused `go test -race` of its steps, and fixes what they show; then it commits once (the pre-commit hook runs the whole `make check`). A task is one commit unless the gate itself fails.
- **Spelling.** US spelling in identifiers and stored values (`canceled`, `recognized`): the linter enforces it (Ruling 97).
- **One write transaction per read-modify-write** in `internal/store`: opening or updating a review, closing one (with the expiry of the task's assumptions and, for a drop, the task's status), each push claim and result, spending a token, the restore claim.
- **Real dependencies in tests.** SQLite files under `t.TempDir()`; a real tmux server on socket `curia-test-<pid>` started with `-f /dev/null`; real git repositories under `t.TempDir()` with `gitEnv`, a local bare repository as the remote; `net/http/httptest` servers on loopback for ntfy and for the endpoint; the built binary in `e2e/`. Fakes only for what cannot run in CI: `tmux.Fake`, the `ui` source interfaces, the seams for `gh`, `claude` and the diff tools. Claude Code is replaced by Plan 3's stub and by recorded screens under `testdata/`. Timing-sensitive code takes an injected clock and, where it sleeps, an injected sleep.
- **One file per command** in `cmd/curia/`, registered from that file's `init()`. Tasks 15 to 19 add to package `main` in parallel: every new package-level name starts with its file's prefix (`push`, `review` but never `reviews`, `reviews`, `restore`, `serve`), in tests too.
- **Shared files** (`go.mod`, `go.sum`, `Makefile`, `.golangci.yml`, `lefthook.yml`, `scripts/`, `.github/`, `cmd/curia/main.go`, `cmd/curia/main_test.go`, `AGENTS.md`, `PROGRESS.md`, `docs/`) are changed only by the orchestrator, in section 6.
- **Timestamps** in new columns use the store's fixed-width UTC layout; a zero time is NULL. Lint limits, `gosec` rules and the ban on suppressions are unchanged; `go mod tidy` is still never run.

## 3. Review focus

What the final whole-branch reviewer must check for this plan in particular. Each has a named test in the task that owns the code.

1. **An agent approving its own work.** An agent runs `curia review T-1 --approve`, calls a tool that does not exist, or posts to the endpoint. Expected: `review` refuses inside an agent session before it opens the store; the tool surface is exactly six and none approves; the endpoint has one route, for a decision, and none for a review. Task 12: `TestToolSurfaceIsExactlySix`. Task 16: `TestReviewRefusesInsideAgent`. Task 19: `TestServeRoutes`.
2. **Merging the wrong thing, or over my work.** The worktree has uncommitted files, the agent is mid-turn, my checkout is on another branch, my checkout has a local change to a file the branch touches, the merge conflicts, a commit-msg hook rejects the merge commit, the branch has no commits. Expected: each is refused or rolled back with `git merge --abort`, nothing is forced, my local changes are intact, the review stays pending. Task 16: `TestMergeRefusals`, `TestMergeConflictAborts`, `TestMergeHookFailureAborts`, `TestMergeKeepsMyLocalChanges`, `TestNeverForces`.
3. **Agent text reaching GitHub, git or my terminal.** A summary holding escape sequences, `@someone`, a closing fence, 1 MiB of text; a task title holding `@`; a branch name starting with `-`. Expected: the view draws no control rune; the pull-request body holds the summary inside one fence that the summary cannot close and no `@` outside it; the title has no `@`; no value is a git option. Task 5: `TestPRBodyFencesSummary`, `TestPRTitleHasNoMention`. Task 8: `TestReviewTextCannotDrawEscapes`. Task 16: `TestPRArgvIsDirect`.
4. **The endpoint as a way in.** A request with no token, a spent token, a token of another ticket, a 1 MiB body, `GET`, unknown JSON fields, an irreversible ticket, eleven wrong tokens in a minute, a listen address of `0.0.0.0`. Expected: one `401` text for every token failure; one answer per token; `429` after ten failures; the server refuses to start on an address that is neither loopback nor Tailscale. Task 9: `TestUsePushTokenOnce`. Task 19: `TestAnswerAuthTable`, `TestAnswerTokenIsSingleUse`, `TestAnswerRateLimit`, `TestServeRefusesPublicAddress`.
5. **A push that should not go, or goes twice.** Quiet hours; a ticket answered before its push is due; five `curia mcp` processes flushing at once; ntfy down, answering `429`, or redirecting; ntfy not configured. Expected: held until the window ends; dropped when the ticket is no longer open; sent once; retried with backoff at most five times; a redirect is not followed (it would carry the token); with nothing configured, no connection at all. Task 7: `TestNotBeforeTable`, `TestPublishDoesNotFollowRedirect`. Task 9: `TestPushClaimedExactlyOnce`. Task 15: `TestFlushDropsAnsweredTicket`, `TestFlushRetriesThenFails`, `TestNoNtfyConfiguredNoNetwork`.
6. **A stray key in the review view.** A held Enter, a paste, `a` then `y` in one read, `y` after the agent re-completed the task while the prompt was open. Expected: nothing is approved, sent or dropped; a prompt for a review that changed is canceled. Task 8: `TestApproveNeedsConfirmationAfterDwell`, `TestReviewPasteIsNotACommand`, `TestHeldEnterDoesNotSendNote`, `TestPromptCanceledWhenReviewChanges`.
7. **Restore bringing back the wrong sessions.** A session that ended weeks ago but was never marked ended, a session of another tmux server, a session that is alive, a teammate, two `curia restore` at once, a stored session id or tmux session name holding `;` or a line break. Expected: only sessions whose transcript changed within the window, whose pane is gone and whose server is this one are offered; nothing happens without `--yes` or a typed `y`; one restore runs at a time; a bad id is skipped and no stored text reaches tmux unvalidated. Task 18: `TestPlanClassifiesEverySession`, `TestRestoreNeedsConfirmation`, `TestRestoreSecondRunRefused`, `TestRestoreNeverPassesStoredTextToTmux`.
8. **Two tmux servers.** A view run in server B while agents live in server A. Expected: A's sessions are not marked stale, are not restored into B, and nothing is typed into B's pane of the same id. Task 10: `TestReconcileLeavesOtherServerAlone`. Task 14: `TestAnswerNotTypedAcrossServers`. Task 18: `TestPlanClassifiesEverySession`.
9. **Hook budget.** Expected: the hook starts no new process and runs no new statement; its latency tests pass unchanged. Task 13: `TestSessionStartStillOneTmuxCall`, `TestHookLatencyUnchanged`.
10. **A stale board.** The agent completes, I ask for changes, it completes again, I approve; or the task is dropped while a review waits. Expected: `review`, `running`, `review`, `done` with no command having to run; a dropped task's review is not counted in the badge. Task 11: `TestDeriveWithReviews`. Task 6: `TestPendingReviewsSkipsDroppedTasks`.
11. **Old and new binaries on one database.** A hook or `curia mcp` started before the upgrade keeps running. Expected: Plan 3's upsert leaves `tmux_socket` alone; an old `curia mcp` simply has five tools. Task 6: `TestMigrationOnPlan3Database`, `TestOldUpsertKeepsSocket`.

## 4. Decided here

Implementers do not choose any of this. Reasons and costs are in section 13.

### 4.1 Migration `internal/store/migrations/0005_reviews.sql`

Exact content. Verified at the desk with the system `sqlite3` 3.51.0 after applying 0001 to 0003 and Plan 3's 0004 as its plan gives it: it runs as one script in one transaction; `'R-' || (SELECT COALESCE(MAX(n), 0) + 1 FROM reviews)` with `RETURNING id` yields `R-1`; a second pending review of one task fails on `reviews_one_pending`; `tests_status = 'ok'` fails its `CHECK`; `decisions_fts` passes `integrity-check` afterwards. Task 6 proves it again on the pinned `modernc.org/sqlite`.

```sql
-- Plan 4: reviews, the push outbox, and the tmux server of a session.
-- Differences from spec section 5 are listed in the plan (section 4.1).

CREATE TABLE reviews (
  n            INTEGER PRIMARY KEY,           -- allocation order
  id           TEXT NOT NULL UNIQUE,          -- 'R-' || n
  task_id      TEXT NOT NULL,
  session_id   TEXT NOT NULL DEFAULT '',      -- the session that called complete_task
  summary      TEXT NOT NULL,                 -- the agent's own completion summary
  tests_status TEXT NOT NULL CHECK (tests_status IN ('passed','failed','not_run')),
  tests_detail TEXT NOT NULL DEFAULT '',      -- the agent's one line about the tests
  status       TEXT NOT NULL CHECK (status IN ('pending','approved','changes_requested','merged','dropped')),
  approved_how TEXT NOT NULL DEFAULT '' CHECK (approved_how IN ('','merge','pr')),
  diff_stat    TEXT NOT NULL DEFAULT '',      -- git diff --stat as approved; '' until then
  head         TEXT NOT NULL DEFAULT '',      -- the branch's commit when approved
  pr_url       TEXT NOT NULL DEFAULT '',
  my_notes     TEXT NOT NULL DEFAULT '',      -- the note of a change request
  note_message TEXT NOT NULL DEFAULT '',      -- M-<n> that carries the note to the agent
  closed_via   TEXT NOT NULL DEFAULT '' CHECK (closed_via IN ('','popup','ssh','cli')),
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL,
  closed_at    TEXT
);
-- One review of a task waits at a time; a second complete_task updates it.
CREATE UNIQUE INDEX reviews_one_pending ON reviews(task_id) WHERE status = 'pending';
CREATE INDEX reviews_task ON reviews(task_id, n);

CREATE TABLE pushes (
  n          INTEGER PRIMARY KEY,
  kind       TEXT NOT NULL CHECK (kind IN ('decision','review')),
  ref        TEXT NOT NULL,                   -- D-<n> or R-<n>
  device     TEXT NOT NULL,                   -- a name from [ntfy.devices]
  status     TEXT NOT NULL CHECK (status IN ('pending','sending','sent','failed','dropped')),
  not_before TEXT NOT NULL,                   -- quiet hours move it to their end
  attempts   INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  token_hash TEXT NOT NULL DEFAULT '',        -- SHA-256 (hex) of the one-time action token; '' without buttons
  created_at TEXT NOT NULL,
  claimed_at TEXT,
  sent_at    TEXT,
  used_at    TEXT                             -- when its token answered the ticket
);
CREATE UNIQUE INDEX pushes_once ON pushes(kind, ref, device);
CREATE INDEX pushes_due ON pushes(not_before) WHERE status IN ('pending','sending');
CREATE UNIQUE INDEX pushes_token ON pushes(token_hash) WHERE token_hash != '';

-- Which tmux server a session's pane is on (#{socket_path}); NULL when unknown or outside tmux.
ALTER TABLE sessions ADD COLUMN tmux_socket TEXT;
```

Differences from the spec. `reviews`: `n` and a `UNIQUE` id, as the other tables have; `tests_status` is an enum with `tests_detail` beside it; `status` gains `dropped`; `approved_how`, `head`, `pr_url`, `note_message`, `closed_via`, `updated_at` are added; `diff_stat` is written when the review is approved, not when it is opened, because the MCP server never runs git (Ruling 70). `pushes` and `sessions.tmux_socket` are new. `tasks` is not altered: `review` and `done` are derived (section 4.4), so its `CHECK` stays. `decisions.answered_via` has allowed `'ntfy'` since migration 0002.

`store.upsert` gains `tmux_socket` (an empty string as NULL) and still names its columns, so an older binary's upsert leaves it alone.

### 4.2 Ids and bounds

`R-<n>`, decimal, no padding, from 1, allocated inside the creating transaction like `D-<n>`. Accepted from outside only when it matches `^R-[1-9][0-9]{0,8}$`.

| Text | Bound | Cleaner |
| --- | --- | --- |
| summary | 1 to 2000 runes (`task.MaxSummary`) | `sanitize.Text` |
| tests detail | 0 to 200 runes (`task.MaxTestsDetail`) | `sanitize.Line` |
| review note | 1 to 600 runes (`model.MaxReviewNote`) | `sanitize.Text` |
| pull-request body | at most 8 KiB (`task.MaxPRBody`) | built by `task.PRBody` |

### 4.3 `complete_task`

The sixth MCP tool, after `check_messages`. Description: `Tell the user the task is finished and ready for review. Call it once everything is committed, with a short summary and the state of the tests.` Schema: `{"type":"object","required":["summary","tests_status"],"properties":{"summary":{"type":"string"},"tests_status":{"type":"string","enum":["passed","failed","not_run"]},"tests_detail":{"type":"string"}}}`.

In order:

1. A caller whose `TeamRole` is `teammate` gets the tool error `ask_lead`: `You are a teammate in an agent team. Tell your team lead you are done; only the lead completes the task.`
2. A caller with no session or no `TaskID` gets `no_task`: `This session is not linked to a Curia task, so there is nothing to complete. End your turn with a summary instead.`
3. The arguments are cleaned and checked (section 4.2); every problem is reported together as `invalid_arguments`.
4. `store.CompleteTask`: a task that does not exist or is stored `dropped` gives `no_task` with `Task T-42 does not exist or was dropped.` Otherwise, in one write transaction, the task's pending review is updated (summary, tests, session, `updated_at`) or a new pending review is inserted.
5. For a new review only, `Deps.Reviewed` is called (the push, section 4.7).

Result: `{"review":"R-3","task":"T-42","updated":false,"instruction":"The review is waiting for the user. Make sure everything is committed, then end your turn. Do not merge or push. If the user asks for changes, their note arrives as a message."}`.

The tool reads no file and runs no program. What it stores is the agent's claim and is shown as such (`tests: passed (agent's report)`).

### 4.4 Task status with reviews

`task.Derive` gains the newest review of the task (the one with the highest `n`); the table replaces Plan 3's section 4.4.

| Order | Condition | Shown |
| --- | --- | --- |
| 1 | stored `dropped` | `dropped` |
| 2 | the newest review is `pending` | `review` |
| 3 | the newest review is `approved` or `merged` | `done` |
| 4 | at least one live linked session, and one of them is `needs_decision` or the task has an open blocking ticket | `needs_decision` |
| 5 | at least one live linked session | `running` |
| 6 | otherwise | the stored status |

A newest review that is `changes_requested` or `dropped` changes nothing: the task shows `running` while its agent lives. A `complete_task` after an approval opens a new review, so the task shows `review` again.

### 4.5 What a review shows

`curia review` builds a `ui.ReviewDetail` per review. Top to bottom:

1. Head line: `R-3 · T-42 Fix the auth refresh · api-server · curia/T-42-fix-the-auth-refresh → main · waiting 12m`.
2. In red, one line per open ticket of the task (`decisions.task_id`): first open assume tickets, `D-9 unconfirmed assumption: <question, 100 runes> → proceeding with: <assumption, 100 runes>`, then open blocking tickets, `D-11 open blocking ticket: <question>`.
3. `Summary (the agent's):` and the summary.
4. `Tests (the agent's report): passed` and the detail when there is one.
5. `Diff against main:` and the output of `git -C <repo> diff --no-color --stat=<width> <base>...<branch>` (three dots: the changes since the branch left its base), at most 64 KiB and 200 lines, through `sanitize.Block`; then a line `… N more lines` when cut. Under it, when it applies, one warning: `the worktree has uncommitted changes that are not in this diff`, `branch <b> does not exist`, or `the branch has no commits beyond <base>`.
6. `Decisions on this task:` one line per decided ticket, `D-4 answered: <question, 80 runes> → <answer, 60 runes>`.
7. `Sessions: 3 (lead and 2 teammates) · duration 1h12m · cost $4.12`, or `cost not recorded` while no linked session has a cost (nothing writes `sessions.cost_usd` until Plan 5). Duration runs from the earliest start of a linked session to the review's creation.
8. The keys and what `a` will do: `a merge into main` or `a push and open a pull request`.

Every string is drawn through `sanitize`; the view is usable at 40 columns.

### 4.6 Approving, requesting changes, dropping

All three act only on a `pending` review, are user-only, and are recorded with `closed_via` (`popup`, `ssh` or `cli`, as `decideVia()` and the flag form give it).

**Approve** (`a`, or `curia review <id> --approve [--merge|--pr]`; the default is `review.approve`). In order; any refusal is one line, changes nothing and leaves the review pending:

1. The task has a branch; `<branch>` and `<base>` resolve in `<repo>`; both pass `task.ValidBranch`.
2. No live linked session is `working` or `waiting`: `the agent is still working; wait for it to stop`.
3. When the worktree is on disk, it is clean (`worktreeDirty`): `uncommitted changes in <worktree>: ask the agent to commit them, or commit them yourself`.
4. `git rev-list --count <base>..<branch>` is above 0: `nothing to merge: <branch> has no commits beyond <base>`.
5. The diff stat of section 4.5 and `git rev-parse <branch>` are taken now, as the record of what was approved.
6. **Merge:** `git -C <repo> symbolic-ref --short -q HEAD` must print `<base>`: otherwise `<repo> has <x> checked out, not <base>: check out <base> there, or approve with --pr`. Then `git -C <repo> merge --no-ff --no-edit <branch>`, limit 2 minutes. Git itself refuses when a local change of mine would be overwritten, and runs the repository's merge and commit-msg hooks. On any failure, if `git rev-parse -q --verify MERGE_HEAD` succeeds, `git merge --abort` runs; the error is git's stderr through `sanitize.Line(…, 300)` followed by `; nothing was merged`. On success `store.CloseReview` records `merged`.
7. **Pull request:** `gh` must be on `PATH`. `git -C <repo> push --set-upstream <remote> <branch>:<branch>` (`review.remote`, default `origin`), limit 2 minutes; then `gh pr create --base <base> --head <branch> --title <title> --body <body>` run in `<repo>`, limit 60 seconds, with `task.PRTitle` and `task.PRBody`. The last line of its stdout is kept as the URL only when it matches `^https://[A-Za-z0-9._/-]{1,200}$`. On success `store.CloseReview` records `approved` with the URL. A failure after the push leaves the branch pushed and the review pending; approving again is safe.
8. `CloseReview` for `approved` or `merged` also expires the task's open assume tickets, in the same transaction (F8: "or when its task merges"). Blocking tickets are not touched.

Outcome lines: `R-3 approved: merged curia/T-42-fix-the-auth-refresh into main (abc1234). 2 unconfirmed assumptions expired. Remove the worktree and branch with: curia clean T-42` and `R-3 approved: pull request https://… opened for curia/T-42-…`. Curia does not clean, push the base, or stop the agent.

**Request changes** (`c`, or `--request-changes --note <text>` or `--note -` for stdin). The note is cleaned (`task.NormalizeNote`). `CloseReview` records `changes_requested` with the note. The target session is the task's first live linked session that is not a teammate (`store.TaskSessions`), else the review's `session_id`; with neither, nothing is sent. Otherwise a message is stored (`store.CreateMessage`: kind `instruction`, sender `user`, `routed_by` `address`, status `queued`, the task, its repo, text `task.NoteMessage`), its id is written to `reviews.note_message`, and `deliver.Message` is called once. Outcome lines: `R-3: changes requested; M-12 delivered (send_keys)`, `R-3: changes requested; M-12 queued: <note>`, `R-3: changes requested; the note is saved but T-42 has no agent session to send it to`. A message queued for a session that is not live waits for that session id: `curia resume` and `curia restore` bring it back under the same id, and it is then handed over at the agent's next Stop or by `check_messages`.

The message text is `Review R-3 of T-42: changes requested. <note>`, so the agent's typed line reads `Message M-12 from the user: Review R-3 of T-42: changes requested. <note>`. Plan 3's Ruling 100 found that a model may refuse a long text handed over by the Stop hook as "from the hook"; the note is capped at 600 runes for that reason too, and live check A sends one by the Stop hook to see that it is followed.

**Drop** (`x`, or `--drop`). `CloseReview` records `dropped` and sets the task's stored status to `dropped` in the same transaction. Outcome line: `R-3 dropped; T-42 is dropped. Its agent is not stopped.`

**Diff** (`o`). The configured tool, run with the terminal attached, without a shell, with `gitEnv()`:

| `review.diff_tool` | Command |
| --- | --- |
| `git` (default) | `git -C <repo> diff <base>...<branch>` |
| `delta` | `git -C <repo> -c core.pager=delta diff <base>...<branch>` |
| `lazygit` | `lazygit -p <worktree when it is on disk, else repo>` |
| `nvim` | `git -C <repo> difftool --no-prompt --tool=nvimdiff <base>...<branch>` |

### 4.7 Pushes

**What is pushed.** A new blocking ticket and a new review (not an update of a pending one), one push per configured device. Assume tickets are never pushed. Nothing is pushed unless `[ntfy] url` is set and `[ntfy.devices]` has an entry.

**Outbox.** `pushEnqueue` inserts one `pushes` row per device with `not_before = notify.NotBefore(now, quiet)`; a row for the same `(kind, ref, device)` is never inserted twice. `pushFlush` then sends what is due:

1. `store.PushesDue` (a read on the partial index); nothing due means no write.
2. `store.ClaimPush`, one write transaction: the oldest row that is `pending` with `not_before <= now`, or `sending` and claimed more than 2 minutes ago (a sender that died), becomes `sending` with `attempts + 1`, `claimed_at = now` and a new `token_hash`.
3. The row's subject is read again. A ticket that is no longer open, a review that is no longer pending, or a device that is no longer configured: `dropped`.
4. `notify.Publisher.Publish`, limit 5 seconds. Success: `sent`. `notify.ErrRejected` (a 4xx other than 429): `failed`, no retry. Any other error: `failed` when `attempts` has reached 5, else `pending` again with `not_before = now + 30 s × 2^(attempts − 1)`, at most 15 minutes.

**Who flushes.** Each `curia mcp` whose configuration has ntfy on runs `pushLoop`: a flush at once when a tool call enqueued something, and one every 60 seconds, which is what sends a push when quiet hours end and retries a failed one. `curia serve` runs the same loop. No other process sends. A push held back while no agent session and no `curia serve` is alive is sent when the next one starts.

**Quiet hours.** `[ntfy] quiet = "22:00-07:00"`, local time, start inclusive and end exclusive, across midnight when the start is later than the end. A push enqueued inside the window is due at the window's end. Quiet hours hold back only ntfy pushes; the bell and the macOS notification of Plan 2 are unchanged.

**Request.** `POST <url>` (the server's base URL) with `Content-Type: application/json`, `Authorization: Bearer <CURIA_NTFY_TOKEN>` when that variable is set, and the JSON body ntfy documents for publishing (assumed until probe N1 records a real accepted request):

```json
{"topic":"<device topic>","title":"D-117 api-server: decision needed","message":"…","priority":4,
 "actions":[{"action":"http","label":"B: Redis stream","url":"<public_url>/v1/answer","method":"POST",
             "headers":{"Authorization":"Bearer <one-time token>","Content-Type":"application/json"},
             "body":"{\"decision\":\"D-117\",\"key\":\"B\"}","clear":true}]}
```

**Ticket wording.** Title `sanitize.Line("<id> <repo base>: decision needed", 80)` (the repo part left out when empty, as Plan 2's `repoPart` does). Message, through `sanitize.Text(…, 1200)`: the question; one line per option, `<key>: <label>`; `Recommended: <key>`; then `Irreversible: answer with curia decide <id>.` for an irreversible ticket, or `More options: curia decide <id>.` when there are more options than buttons. Priority 4.

**Buttons.** Only when `[serve] public_url` is set and the ticket is not irreversible: at most three (`notify.MaxActions`, ntfy's limit), the recommended option first and then the others in ticket order; label `sanitize.Line("<key>: <label>", 30)`; all buttons of one push carry the same one-time token. The token is 32 random bytes, base64url without padding (43 characters).

**Review wording.** Title `R-3 T-42: ready for review`; message: the task title, `Tests (the agent's report): passed`, and the summary cut to 600 runes; priority 3; no buttons.

### 4.8 `curia serve`

`curia serve [--listen <ip:port>]`, user-only, long-running, started by me (nothing installs or starts it). It refuses to start when `[serve] public_url` is empty. The listen address is the flag, else `[serve] listen`, else the first address of this machine in `100.64.0.0/10` (then `fd7a:115c:a1e0::/48`) with port 8787; an address that is neither loopback nor in those two ranges is refused, as is finding none. There is no TLS: the Tailscale tunnel carries it, or `tailscale serve` in front of a loopback listener.

`http.Server` with `ReadHeaderTimeout` 5 s, `ReadTimeout` 10 s, `WriteTimeout` 30 s, `IdleTimeout` 60 s, `MaxHeaderBytes` 8 KiB. Routes:

| Request | Behaviour |
| --- | --- |
| `GET /healthz` | `200`, body `ok`. No data. |
| `POST /v1/answer` | below |
| anything else | `404`; a known path with another method `405` |

`POST /v1/answer`, in order:

1. When 10 token checks have already failed in the current minute (one counter for the server, injected clock): `429 {"error":"too many attempts"}`.
2. `Content-Type` must be `application/json`; the body is at most 1 KiB; it must be exactly `{"decision":"<id>","key":"<key>"}` with no other field, `decision.ValidID` and a key matching `^[A-Za-z0-9]{1,8}$`: otherwise `400 {"error":"bad request"}`.
3. The `Authorization` header must be `Bearer ` and 43 characters of `[A-Za-z0-9_-]`; `store.UsePushToken(hash, decision id, now)` must succeed. It does so once: for a `decision` push that was `sent` at most 24 hours ago, whose `ref` is that decision and whose `used_at` is not set, and it sets `used_at`. Every failure here is `401 {"error":"unauthorized"}`, the same bytes whatever the reason, and counts for step 1.
4. An irreversible ticket: `422 {"error":"answer this decision with curia decide"}`.
5. `deliver.Answer(ctx, st, tm, log, id, model.Answer{Key: key, Via: model.AnsweredNtfy}, now)`. `store.ErrNotOpen` or `store.ErrNotFound`: `409 {"error":"D-117 is no longer open"}`. `decision.ErrBadAnswer`: `422 {"error":"unknown option"}`. Any other error: `500 {"error":"internal"}`, with the cause in the log only.
6. `200 {"decision":"D-117","status":"answered","delivery":"<delivered, or the outcome's note through sanitize.Line(…, 200)>"}`.

Each request is logged with method, path, status and decision id, never the token or the body. `curia serve` also runs `pushLoop`. `SIGINT` or `SIGTERM` shuts it down within 5 seconds.

### 4.9 tmux server identity

- `tmux.Pane` gains `Socket`, from `#{socket_path}` in the format `ListPanes` and `PaneInfo` already use. A value is kept only when it is an absolute path of at most 256 bytes with no rune below `0x20`.
- On a session's first event the hook stores the socket of the server its pane is on (`sessions.tmux_socket`), from the `PaneInfo` call it already makes. Rows written before the upgrade have none until their next SessionStart; an empty socket means "unknown" everywhere below and behaves as today.
- `fleet.Reconcile` never marks stale a session whose socket is known and differs from the socket of the panes it was given: that session lives on another server.
- `deliver.Answer` and `deliver.Message` type nothing for a session whose socket is known and differs from the listed panes' socket; the answer or message stays queued and the note says `the agent is on another tmux server (<base name of its socket>); nothing typed`, followed by the usual `tell the agent to call …`. The check comes before the pane is captured.
- `curia restore` offers only sessions whose socket is unknown or is the server it runs against.

### 4.10 `curia restore`

`curia restore [--dry-run] [--yes] [--since <duration>]`, user-only. `--since` defaults to `restore.max_age` (24 hours).

1. `ListPanes` (2 s). When no tmux server answers: exit 1, `no tmux server is running: start tmux, then run curia restore again`. The target socket is the socket of the listed panes.
2. `store.ClaimRestore(now, 2 minutes)`; when another restore holds it: exit 1, `another curia restore is running`. Released when the command ends.
3. Every session that is not `ended` is classified, in this order, by the pure function `restorePlan`:

| Class | Condition | Action |
| --- | --- | --- |
| live | its pane is listed and the session did not start before the server did | none; not printed |
| other server | its socket is known and is not the target socket | skip |
| outside tmux | it has no pane | skip |
| teammate | `team_role` is `teammate` | skip; set `ended` when its lead is restored |
| not Claude Code | `agent` is not `claude` | skip |
| bad id | its id does not match `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$` | skip |
| paused | its task has `paused_at` set | skip, `use curia resume T-42` |
| no directory | its worktree, or its cwd when it has none, is not a directory | skip |
| no transcript | its transcript path is not a regular file | skip |
| too old | the transcript was last modified before `now − since` | skip, `last active 3d ago; use --since` |
| superseded | another candidate of the same task was active more recently | skip |
| restore | none of the above | restore, at most 32 a run, most recently active first; the rest are skipped as `over the limit; run again` |

4. The plan is printed as a table (`SESSION`, `WINDOW`, `TASK`, `REPO@BRANCH`, `LAST ACTIVE`, `ACTION`), every cell through `ui.Cell(sanitize.Line(…))`. `--dry-run` stops here, exit 0. Without `--yes`, on a terminal it asks `restore 3 sessions? [y/N]` and continues only on `y`; without a terminal it exits 1 and says to pass `--yes`.
5. Each session to restore gets one window, opened with `tm.NewWindow`:

| | Session linked to a task that exists and is not dropped | Any other session |
| --- | --- | --- |
| tmux session | `task.SessionName(task.Repo)` | the stored tmux session name with every run of runes outside `A-Za-z0-9_-` replaced by one `_`, cut to 40; `curia` when empty |
| window | the task id | the base name of the repo (of the cwd when there is no repo), cleaned the same way; `agent` when empty |
| directory | the task's worktree | the session's cwd |
| environment | `CURIA_TASK_ID=<id>` | none |
| command | `[<bin>, "launch", <task id>, "--resume", <session id>]`, then `--model <agent_model>` when the task has one and `--team` when the task has it | `[<absolute path of claude>, "--resume", <session id>]` |

   For a linked task the new pane and `launched_at` are stored on the task. A `NewWindow` failure is reported and the next session is tried. Restored agents run with their normal permission settings: no permission flag is ever passed.
6. For at most 30 seconds, every 250 ms (injected clock and sleep), `restore` waits for each launched session's row to be live in its new pane. When another live session shows up in that pane instead (the resume gave it a new id), the old row is set `ended` and the session counts as restored.
7. For each restored team lead: its teammates' rows are set `ended`, and one message is stored for the lead and offered to `deliver.Message` once the lead is back: `Curia restored this session after a restart. Your teammates did not survive it: respawn your team from the shared task list, then continue.`
8. One line per session: `restored 1a2b3c4d in api-server:T-42 (%3)`, `1a2b3c4d did not report within 30s; look at api-server:T-42`, `could not open a window for 1a2b3c4d: <error>`. Exit 1 when any session it tried did not come back.

`restore` never touches a window that exists, and types nothing itself. It works beside tmux-resurrect: that restores my layout and shells, this adds one window per agent.

### 4.11 Views

- **Badge.** `R<n>` counts pending reviews whose task is not stored `dropped` (`store.PendingReviews`, one statement). `curia status` still prints nothing on any error and calls no tmux.
- **Rank.** `fleet.WithReviews` marks each live session whose task has a pending review (`Session.ReviewPending`, never stored). `fleet.Rank` gains one group, review-ready, after waiting and before idle-unseen: an idle session with `ReviewPending`, seen or not. `fleet.Next` jumps to it. `ls`, `next` and the dashboard mark before they rank.
- **Dashboard.** A review-ready row shows the state icon `◆` in blue. `r` on a row whose task has a pending review opens the review view for that task until it closes; on any other row it says `no review is waiting for this session`; in the sidebar or inside an agent session it says why not. The help line gains `r review`.
- **Board.** Five columns at 100 columns or more: backlog, ready, running, needs decision, review. `done` tasks are hidden until `D` shows them (a sixth column at 120 or more, else the list layout); `D` again hides them. Below 100 columns, one list under the status headings, as before.
- **`curia reviews ls [--status pending|all|<status>] [--json]`.** Table columns `ID`, `AGE`, `TASK`, `STATUS`, `TESTS`, `SUMMARY`; `STATUS` reads `pending`, `approved (pr)`, `merged`, `changes requested`, `dropped`. JSON keys are the column names of section 4.1, times as RFC 3339 or null.
- **Window marker.** `attention.Marker` returns `blue` for a live session whose task has a pending review and that changed since I last focused its pane (`SeenAt` before `UpdatedAt`), unless it is red; red wins within a window. The snippet's `markerFormat` becomes `#{?#{==:#{@curia_marker},red},#[fg=red] ●,#{?#{==:#{@curia_marker},blue},#[fg=blue] ●,}}` (verified at the desk on tmux 3.8: appended through `#{E:@curia_marker_format}` it draws a blue dot, a red dot or nothing). Because the formats only refer to the option, sourcing the new snippet on a running server is the whole upgrade and no format is touched. The marker is applied when `complete_task` opens a review (by `curia mcp`), after every `curia review` action, and wherever it is applied today (`curia seen` on focus, reconcile, the hook on a marker change); focusing the pane clears it, as the spec says.

### 4.12 Config

```toml
[review]
approve   = "merge"        # merge | pr
remote    = "origin"
diff_tool = "git"          # git | delta | lazygit | nvim

[ntfy]
url   = ""                 # empty: no pushes
quiet = ""                 # "22:00-07:00", local time; empty: none
[ntfy.devices]             # device name = topic; empty: no pushes
# phone = "curia-k3j9x2m4"

[serve]
listen     = ""            # ip:port; empty: the first Tailscale address, port 8787
public_url = ""            # what the phone calls; empty: no buttons

[restore]
max_age = "24h"
```

`config.Load` rejects, with an error naming the key: `review.approve` or `review.diff_tool` outside its list; `review.remote` not matching `^[A-Za-z0-9._-]{1,64}$` or starting with `-`; an `ntfy.url` or `serve.public_url` that is over 200 bytes, does not parse, has no host, has user info, a query or a fragment, or is `http` to a host that is not `localhost`, a loopback or Tailscale address, or a name ending in `.ts.net` (ticket text and tokens do not travel in clear over the open network); `ntfy.quiet` that is not `HH:MM-HH:MM` with two different valid times; more than 8 devices, a device name not matching `^[a-z0-9_-]{1,32}$`, a topic not matching `^[A-Za-z0-9_-]{1,64}$`; a `serve.listen` that is not `ip:port` with an IP literal that is loopback or Tailscale and a port from 1 to 65535; `restore.max_age` not above zero. A trailing `/` of either URL is dropped.

### 4.13 New inputs: bound, cleaner, sink

| Input | From | Bound | Cleaned by | Reaches |
| --- | --- | --- | --- | --- |
| summary, tests detail | the agent (`complete_task`) | 2000 and 200 runes | `sanitize.Text`, `sanitize.Line` | store, review view, `reviews ls`, push message, pull-request body inside a fence |
| tests status | the agent | enum of three | exact match | store, views |
| review note | me | 600 runes | `sanitize.Text`; then Plan 3's message path (`sanitize.Keys` when typed) | store, a message |
| review or task id argument | me | 11 bytes | `task.ValidID`, `task.ValidReviewID` | SQL parameter |
| diff stat, git stderr | git | 64 KiB and 200 lines; 300 runes | `sanitize.Block`; `sanitize.Line` | review view, stdout, `reviews.diff_stat` |
| base, branch, remote | the task row, config | Plan 3's patterns; `^[A-Za-z0-9._-]{1,64}$` | `task.ValidBranch`, pattern check | git and `gh` argv, never first, never a shell |
| `gh` stdout | `gh` | 64 KiB | last line kept only if it matches the URL pattern | `reviews.pr_url`, stdout |
| ntfy URL, topics, quiet, listen, public URL | config | section 4.12 | `config.Load` | the HTTP client, the listener, the action URL |
| `CURIA_NTFY_TOKEN` | the environment | 512 bytes, no rune below `0x20`, else ignored | check | one request header |
| ntfy's reply | the ntfy server | 4 KiB read, then discarded | never parsed | a status code only |
| ticket text in a push | the agent | 1200 runes, labels 30 | `sanitize.Text`, `sanitize.Line`, JSON-encoded | ntfy, my phone |
| HTTP request | the tailnet | 8 KiB headers, 1 KiB body | strict JSON decode, id and key patterns, token pattern | `deliver.Answer` |
| stored session row (id, tmux session name, cwd, repo, transcript path) | the store, written by hooks | the patterns of section 4.10 | pattern check or character replacement | `NewWindow` (validated again there), `os.Stat` |
| socket path | tmux | 256 bytes | absolute, no control rune | a compared string, the last element in a note through `sanitize.Line` |

## 5. File structure

Module `github.com/ZachFara/curia`. No package is added, so the package order, depguard and the layout check stay as Plan 3 leaves them.

| Path | Responsibility | Task |
| --- | --- | --- |
| `internal/model/` | Plus `review.go` (`Review`, `Push`, their enums, `MaxReviewNote`); `Session` gains `Socket` and `ReviewPending`; `AnsweredNtfy`. | 1 |
| `internal/config/` | Plus `Review`, `Ntfy`, `Serve`, `Restore`, `TailscaleAddr`, `ParseQuiet`. | 1 |
| `internal/task/` | Plus `review.go` (completion and note rules, pull-request title and body); the brief's last rule; `Derive` and `Cards` take reviews. | 5, 11 |
| `internal/store/` | Plus migration `0005_reviews.sql`, `reviews.go`, `pushes.go`; changes to `store.go`, `housekeeping.go`. | 6, 9 |
| `internal/hook/` | `Env.PaneInfo` also returns the socket. | 13 |
| `internal/tmux/` | `Pane.Socket`. | 2 |
| `internal/fleet/` | `WithReviews`, the review-ready group, the other-server rule of `Reconcile`. | 10 |
| `internal/attention/` | The blue marker. | 10 |
| `internal/notify/` | Plus `ntfy.go`: quiet hours, push wording, tokens, the publisher. | 7 |
| `internal/deliver/` | The other-server refusal. | 14 |
| `internal/mcp/` | Plus `complete.go`: the sixth tool. | 12 |
| `internal/ui/` | Plus `review.go`, `review_view.go`; the board's review and done columns; the dashboard's `r` key and review-ready icon. | 4, 8, 20 |
| `internal/install/` | The sixth tool rule; the skill text; the marker format's value. | 3, 10 |
| `cmd/curia/` | New files `push.go`, `review.go`, `review_git.go`, `reviews.go`, `restore.go`, `serve.go`; changes to `task.go`, `mcp.go`, `hook.go`, `ui.go`, `common.go`, `status.go`, `install.go`. | 3, 11, 13, 15 to 21 |
| `testdata/reviews/` **new** | Hand-written MCP requests and config for the acceptance test. | 22 |
| `e2e/` | Plus `reviews_test.go` and its golden files. | 22 |

## 6. Setup (orchestrator, before Wave 1)

One PR from `chore/plan4-setup` into `main`, after `plan-3` has merged, the dependency table above has been checked, and the probes of section 14 have run; `plan-4` is branched from `main` once it merges. No task touches these files afterwards.

1. **`go.mod`, `go.sum`, `Makefile`, `lefthook.yml`, `.golangci.yml`, `scripts/check-layout.sh`, `.goreleaser.yaml`:** no change. No module and no package is added; `testdata/reviews/`, `e2e/testdata/reviews/` and `internal/notify/testdata/` sit in allowed places.
2. **`.github/workflows/ci.yml`:** add `plan-4` to `branches`. **`README.md`:** the command list gains `curia review`, `curia reviews`, `curia restore` and `curia serve`, and a short section on ntfy and the endpoint (off by default, the config keys of section 4.12, `CURIA_NTFY_TOKEN`); written when Wave 5 has merged, in the PR of `plan-4` into `main`.
3. **`cmd/curia/main_test.go`:** `guardUnset` gains `CURIA_NTFY_TOKEN`. Three new seams start real programs or reach outside: `reviewGH` (Task 16), `restoreLookPath` (Task 18) and `reviewDiffLookPath` (Task 16). Each is declared by its own task, and that task's test file replaces it with a default that fails any test which has not set its own, as Plan 2 required of `claudeRunner`.
4. **`AGENTS.md`:**
   - "Sources of truth": this plan is the current plan; Plan 3 is complete.
   - "What Curia is": one sentence each for reviews and `complete_task` (six tools), `curia review` and what approving does, pushes and the outbox, `curia serve`, `curia restore`.
   - Toolchain rule: "during Plans 1 to 4". Conventions: Plan 4 task branches are `feat/plan4-tN-<slug>`, worktrees `.worktrees/plan4-tN-<slug>`. A new line: there is no per-task review in Plan 4; a task merges when its named tests and `make check` pass and CI is green, and it is one commit (section 2).
   - "Things learned while building", new entries: `git merge` runs the repository's commit-msg hook on the merge commit, and a failed hook leaves `MERGE_HEAD` behind, which `git merge --abort` clears (git 2.56.0, at the desk); `git merge` with an unrelated local change in the checkout succeeds and keeps the change, and refuses when the change overlaps; `git diff --stat <base>...<branch>` and `git push <remote> <branch>:<branch>` work from the main checkout without the worktree; `#{socket_path}` names the tmux server and a session's `tmux_socket` is written only at its start; a derived `review` or `done` comes from the newest review and is never stored on the task; the action token of a push is stored only as its SHA-256; whatever section 14's probes establish, each with its Claude Code, tmux and ntfy version.
5. **Decision records:** `docs/decisions/0021-reviews-and-derived-done.md` (sections 4.1, 4.3, 4.4, 4.6); `0022-pushes-outbox-and-one-time-tokens.md` (sections 4.7, 4.8, decisions 9 to 16); `0023-tmux-server-identity.md` (section 4.9); `0024-restore-by-resume.md` (section 4.10 and the probes behind it). If Plan 3 used other numbers, these take the next free ones.
6. **Fixtures recorded from the real programs,** committed by setup and never generated by a task: `internal/notify/testdata/ntfy/publish-accepted.json` and `publish-reply.json` (probe N1: one request ntfy accepted, with the token replaced by `TOKEN`, and its reply); `internal/deliver/testdata/prompt/p4-idle-after-resume.txt` and any dialog probe R1 saw.
7. **`PROGRESS.md`:** a Plan 4 section: the waves of section 7 with done criteria per task, a ledger, Rulings from N (a placeholder: the orchestrator fills in the first number free after Plan 3's last ruling), the probes' results, the owner's questions of section 15. Mark as addressed: Plan 2 final-review M7 and Plan 1 Open Question 13 (Tasks 2, 6, 10, 13, 14, 18); F8's merge expiry and review link (Tasks 6, 8); the `R` count Plan 1 left at zero (Task 21).
8. **Migration numbering:** `0005_reviews.sql` is this plan's only migration and Task 6 creates it (see D1). A migration found necessary later is created by the orchestrator in a fix wave.

## 7. Waves

Each task runs on its own branch and worktree and merges by PR into `plan-4`. A task consumes only what an earlier wave merged, or what `main` holds after Plan 3. Two tasks in one wave never create or modify the same file. Counts in brackets are files, tests and fixture directories included. Appendix A is the check of both rules.

| Wave | Task | Model | Creates | Modifies |
| --- | --- | --- | --- | --- |
| Before 1 | orchestrator | | section 14's probes, then section 6 | shared files |
| 1 | 1 Types and config | Sonnet | `internal/model/review.go` | `internal/model/model.go`, `decision.go`, `internal/config/config.go`, `config_test.go` [5] |
| 1 | 2 tmux: the server's socket | Sonnet | none | `internal/tmux/tmux.go`, `exec.go`, `tmux_test.go` [3] |
| 1 | 3 Install: sixth tool and the skill | Sonnet | `internal/install/plan4_test.go` | `internal/install/settings.go`, `plan2_test.go`, `skill/SKILL.md`, `cmd/curia/install.go`, `install_test.go` [6] |
| 1 | 4 Board: review and done | Opus | none | `internal/ui/board.go`, `board_view.go`, `board_test.go` [3] |
| 2 | 5 Review rules and the brief | Sonnet | `internal/task/review.go`, `review_test.go` | `internal/task/brief.go`, `brief_test.go`, `e2e/testdata/tasks/stub_argv.golden.txt` [5] |
| 2 | 6 Store: reviews and the session's server | Opus | `internal/store/migrations/0005_reviews.sql`, `reviews.go`, `reviews_test.go` | `internal/store/store.go`, `store_test.go` [5] |
| 2 | 7 ntfy: rules and sender | Opus | `internal/notify/ntfy.go`, `ntfy_test.go` | none [2] |
| 2 | 8 Review view | Opus | `internal/ui/review.go`, `review_view.go`, `review_test.go` | none [3] |
| 3 | 9 Store: pushes and the restore claim | Opus | `internal/store/pushes.go`, `pushes_test.go` | `internal/store/housekeeping.go`, `housekeeping_test.go` [4] |
| 3 | 10 Fleet and attention: review-ready, other servers | Opus | none | `internal/fleet/fleet.go`, `fleet_test.go`, `internal/attention/attention.go`, `attention_test.go`, `internal/install/tmuxconf.go`, `tmux_real_test.go` [6] |
| 3 | 11 Task status from reviews | Sonnet | none | `internal/task/status.go`, `status_test.go`, `cmd/curia/task.go`, `task_test.go` [4] |
| 3 | 12 MCP: `complete_task` | Opus | `internal/mcp/complete.go`, `complete_test.go` | `internal/mcp/tools.go`, `tools_test.go`, `cmd/curia/mcp_test.go`, `e2e/testdata/decisions/tools_list.golden.json` [6] |
| 3 | 13 Hook: record the server | Opus | none | `internal/hook/run.go`, `hook_test.go`, `cmd/curia/hook.go`, `hook_test.go` [4] |
| 4 | 14 Deliver: never type across servers | Opus | none | `internal/deliver/deliver.go`, `deliver_test.go`, `message.go`, `message_test.go` [4] |
| 4 | 15 Push outbox and MCP wiring | Opus | `cmd/curia/push.go`, `push_test.go` | `cmd/curia/mcp.go` [3] |
| 4 | 16 `curia review` | Opus | `cmd/curia/review.go`, `review_test.go`, `review_git.go`, `review_git_test.go` | none [4] |
| 4 | 17 `curia reviews` | Sonnet | `cmd/curia/reviews.go`, `reviews_test.go`, `cmd/curia/testdata/reviews.golden.json` | none [3] |
| 4 | 18 `curia restore` | Opus | `cmd/curia/restore.go`, `restore_test.go` | none [2] |
| 5 | 19 `curia serve` | Opus | `cmd/curia/serve.go`, `serve_test.go` | none [2] |
| 5 | 20 Dashboard: the `r` key | Opus | none | `internal/ui/model.go`, `view.go`, `ui_test.go`, `cmd/curia/ui.go`, `ui_test.go` [5] |
| 5 | 21 Badge and ranking in the commands | Sonnet | none | `cmd/curia/common.go`, `common_test.go`, `status.go`, `status_test.go`, `next_test.go`, `ls_test.go` [6] |
| 6 | 22 Acceptance | Opus | `testdata/reviews/`, `e2e/reviews_test.go`, `e2e/testdata/reviews/` [3] | none |

Dependencies: 5, 6, 7, 8 need 1; 9 needs 6; 10 needs 2, 6; 11 needs 6; 12 needs 5, 6; 13 needs 2, 6; 14 needs 2, 6; 15 needs 7, 9, 12; 16 needs 5, 6, 8; 17 needs 6; 18 needs 2, 6, 9; 19 needs 9, 15; 20 needs 6, 8, 16; 21 needs 6, 10; 22 needs everything.

Why each model. Sonnet: 1, 2, 5, 11, 17, 21 (one to six files of types, pure rules or a thin command over finished store calls, with exact signatures and tests); 3 (wording and one list). Opus: 6, 9 (concurrent write transactions); 13, 14 (the hook path and the typing path); 7, 15, 19 (the network, goroutines, an HTTP surface that is security-sensitive); 16, 18 (process boundaries: git, `gh`, tmux, and judgment about what is safe to merge or relaunch); 4, 8, 20 (UI: Plan 2's decide view took four fix rounds on key handling); 10 (the re-check inside `Reconcile`'s transaction, which Plan 1 raised to Opus, and three packages); 12 (two packages and the tool surface); 22 (cross-process acceptance).

**Where scope was kept down.** Twenty-two tasks against Plan 3's thirty-one, one commit each. No new package, no new module, no change to depguard or the layout check. No new key and no tmux option; the snippet changes in one constant. A change request and the restore notice reuse Plan 3's message path instead of a second delivery mechanism. `review` and `done` are derived, so `tasks` needs no migration. No assume-ticket pushes, no review approval from the phone, no per-repo approve mode, no free-form diff command, no pane layout restore, no pushes listing command, no cost computation (the review shows a stored cost when Plan 5 writes one). Each is recorded in sections 13 and 16.

**Smoke and live checks** (orchestrator, on `plan-4`, temp `HOME` and `CURIA_DB`, a private tmux server; Plan 2's lesson was that every Critical defect came from a live run):

| After wave | Check |
| --- | --- |
| 2 | `make check`; open the wave-2 database with the Plan 3 binary's `curia ls` and `curia task ls`, and the reverse: both work |
| 3 | by hand over a pipe: `tools/list` shows six; `complete_task` from a session with `CURIA_TASK_ID` creates `R-1`; `curia task ls` shows `review`; `curia hook SessionStart` in the private server stores its socket |
| 4 | **live check A** (real Claude Code as section 14 prescribes): a spawned agent finishes a small task and calls `complete_task` unprompted; `curia review` shows it; `c` with a note reaches the agent by typing and, in a second run, by the Stop hook; `a` merges in a throwaway repository; with a local `ntfy serve` (or a throwaway topic) a blocking ticket's push arrives; `curia restore` after `tmux kill-server` brings the agent back and it continues |
| 5 | **live check B:** `curia serve` on loopback answers a real pushed ticket with `curl` and the agent receives the answer; the badge shows `R1` in a real status line; `r` in the real popup, including a held Enter in the note input; the owner's phone check of section 15, question 6, if the owner is present |
| 6 | Task 22, plus `deadcode ./...` without `-test`: only test doubles are reported |

## 8. Tasks 1 to 4 (Wave 1)

### Task 1: Types and config

**Model:** Sonnet (types and validation, exact signatures).

**Files:**

- Create: `internal/model/review.go`
- Modify: `internal/model/model.go`, `internal/model/decision.go`, `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Brief includes:** section 4.12.

**Interfaces:**

- Consumes: nothing.
- Produces (types and constants; the only logic is in `config`):

```go
// internal/model/review.go
type ReviewStatus string // ReviewPending "pending", ReviewApproved "approved", ReviewChanges "changes_requested",
                         // ReviewMerged "merged", ReviewDropped "dropped"
type TestsStatus string  // TestsPassed "passed", TestsFailed "failed", TestsNotRun "not_run"
type ApproveHow string   // ApproveMerge "merge", ApprovePR "pr"
const MaxReviewNote = 600 // runes of a change request
type Review struct {
    N                                          int64
    ID, TaskID, SessionID                      string
    Summary                                    string
    Tests                                      TestsStatus
    TestsDetail                                string
    Status                                     ReviewStatus
    ApprovedHow                                ApproveHow
    DiffStat, Head, PRURL, Notes, NoteMessage  string
    ClosedVia                                  AnsweredVia
    CreatedAt, UpdatedAt, ClosedAt             time.Time
}
type PushKind string   // PushDecision "decision", PushReview "review"
type PushStatus string // PushPending "pending", PushSending "sending", PushSent "sent", PushFailed "failed", PushDropped "dropped"
type Push struct {
    N                                     int64
    Kind                                  PushKind
    Ref, Device                           string
    Status                                PushStatus
    NotBefore                             time.Time
    Attempts                              int
    LastError, TokenHash                  string
    CreatedAt, ClaimedAt, SentAt, UsedAt  time.Time
}
```

  - `model.Session` gains `Socket string` (stored: the tmux server's socket path) and `ReviewPending bool` (never stored; set by `fleet.WithReviews`).
  - `internal/model/decision.go`: `AnsweredNtfy AnsweredVia = "ntfy"`.
  - `config.Config` gains `Review config.Review` (`toml:"review"`), `Ntfy config.Ntfy` (`toml:"ntfy"`), `Serve config.Serve` (`toml:"serve"`), `Restore config.Restore` (`toml:"restore"`):

```go
type Review struct {
    Approve  string `toml:"approve"`   // "merge" (default) or "pr"
    Remote   string `toml:"remote"`    // default "origin"
    DiffTool string `toml:"diff_tool"` // "git" (default), "delta", "lazygit", "nvim"
}
type Ntfy struct {
    URL     string            `toml:"url"`
    Quiet   string            `toml:"quiet"`
    Devices map[string]string `toml:"devices"` // never nil
}
type Serve struct {
    Listen    string `toml:"listen"`
    PublicURL string `toml:"public_url"`
}
type Restore struct {
    MaxAge time.Duration `toml:"max_age"` // default 24h
}
// Enabled reports whether pushes are on: a URL and at least one device.
func (n Ntfy) Enabled() bool
// DeviceNames returns the device names, sorted.
func (n Ntfy) DeviceNames() []string
// ParseQuiet reads "HH:MM-HH:MM" as minutes after midnight. ok is false for "" and for anything else.
func ParseQuiet(s string) (start, end int, ok bool)
// TailscaleAddr reports whether a is in 100.64.0.0/10 or fd7a:115c:a1e0::/48.
func TailscaleAddr(a netip.Addr) bool
```

  `Load` applies section 4.12's rules, each error naming its key (`review.approve`, `ntfy.url`, `ntfy.devices.<name>`, `serve.listen` and so on).

- [ ] **Step 1: Write the failing tests**

```go
func TestPlan4Defaults(t *testing.T)        // Default(): approve "merge", remote "origin", diff_tool "git", ntfy URL "" and Devices non-nil and empty, serve both "", restore 24h
func TestLoadPlan4Overrides(t *testing.T)   // every key of section 4.12 set -> read back; "https://ntfy.sh/" and a public_url with a trailing "/" lose it
func TestBadReviewConfigRejected(t *testing.T) // approve "squash"; diff_tool "vimdiff; rm"; remote "-x", "a b", 65 runes -> error naming the key
func TestBadNtfyConfigRejected(t *testing.T)
// url "ftp://x", "https://", "https://u:p@ntfy.sh", "https://ntfy.sh?x=1", "https://ntfy.sh#f", "http://example.com", 201 bytes;
// quiet "22:00", "25:00-07:00", "07:00-07:00", "a-b"; 9 devices; device "Phone", "a b"; topic "", "a/b", 65 runes -> error naming the key
func TestPlainHTTPOnlyInsideTailnet(t *testing.T) // http://127.0.0.1:8080, http://localhost:8080, http://100.101.102.103, http://box.tail1234.ts.net accepted for both URLs
func TestBadServeConfigRejected(t *testing.T) // listen "8787", "0.0.0.0:8787", "192.168.1.5:8787", "example.com:80", "100.101.102.103:0", ":8787" -> error naming serve.listen; "127.0.0.1:8787", "100.101.102.103:8787", "[fd7a:115c:a1e0::1]:8787" accepted
func TestNonPositiveRestoreMaxAgeRejected(t *testing.T)
func TestParseQuiet(t *testing.T)           // "22:00-07:00" -> 1320, 420, true; "00:00-23:59" -> 0, 1439, true; "" -> ok false
func TestTailscaleAddr(t *testing.T)        // 100.64.0.0, 100.127.255.255, fd7a:115c:a1e0::1 yes; 100.128.0.0, 10.0.0.1, 127.0.0.1, ::1 no
func TestNtfyEnabledAndDeviceNames(t *testing.T) // URL without devices and devices without URL -> not enabled; names sorted
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/config/ -v`. Expected: FAIL, unknown field `Review`.
- [ ] **Step 3: Implement.** `TestDefaults` gains the Plan 4 assertions. URL checks use `net/url` and `net/netip`; no regular expression is needed for them.
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/model/ ./internal/config/ -v && go build ./...`. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat: review and push types, review, ntfy, serve and restore config"`

**Done when:** the ten tests pass, every existing config test still passes, `go build ./...` works.

### Task 2: tmux: the server's socket

**Model:** Sonnet (one field through an existing parser, with a real-tmux test).

**Files:**

- Modify: `internal/tmux/tmux.go`, `internal/tmux/exec.go`
- Test: `internal/tmux/tmux_test.go`

**Brief includes:** section 4.9, first item.

**Interfaces:**

- Consumes: nothing.
- Produces: `tmux.Pane` gains `Socket string`: "the socket path of the tmux server that holds the pane (`#{socket_path}`), empty when tmux did not report a usable one". `paneFormat` gains a sixth, last field `\t#{socket_path}`; `parsePanes` reads it with a split into at most six parts, so a tab inside the path lands in the last part, and keeps it only when it is absolute, at most 256 bytes, valid UTF-8 and has no rune below `0x20`; otherwise the pane is still returned, with an empty `Socket`. `ListPanes` and `PaneInfo` both carry it. `tmux.Fake` needs no change: it returns the `Pane` values a test gave it.

- [ ] **Step 1: Write the failing tests**

```go
func TestListPanesReportsSocket(t *testing.T)   // real server: every pane's Socket equals `display-message -p '#{socket_path}'` and is absolute
func TestPaneInfoReportsSocket(t *testing.T)    // real server: the same value for one pane
func TestParsePanesSocketRules(t *testing.T)    // a line with a relative path, a 300-byte path, a path holding "\t" or "\x1b" -> the pane with Socket ""; a five-field line (older format) -> the pane with Socket ""
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/tmux/ -run 'Socket' -v`. Expected: FAIL, `Pane` has no field `Socket`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/tmux/ -v`. Expected: PASS, the real-tmux tests not skipped where tmux is installed, every existing test included.
- [ ] **Step 5: Commit.** `git commit -m "feat: report the tmux server's socket with each pane"`

**Done when:** the three tests pass and no existing tmux test changed its assertions.

### Task 3: Install: sixth tool and the skill

**Model:** Sonnet (one list, fixed wording).

**Files:**

- Create: `internal/install/plan4_test.go`
- Modify: `internal/install/settings.go` (`toolRules`), `internal/install/skill/SKILL.md`, `cmd/curia/install.go` (wording)
- Test: `internal/install/plan2_test.go`, `cmd/curia/install_test.go`

**Brief includes:** section 2's "tmux installer rulings".

**Interfaces:**

- Consumes: Plan 3's five tool rules and its skill text (D12); the installer as merged (D18).
- Produces:
  - `install.AllowTools`, `install.ToolsAllowed` and `install.Unhooks` keep their signatures and cover six exact names: the five and `mcp__curia__complete_task`. `curia install` says "the six curia tools" where it said five. An install over a Plan 3 install adds only the missing sixth name, with the usual backup. Nothing else about `curia install` changes: no tmux line, no key, no option.
  - `SKILL.md` gains, directly before the line that starts `You cannot answer a ticket`, exactly:

```markdown
## Finishing a task

If you are working on a Curia task (your first prompt names it, for example `Task T-42`), finish like this: commit everything, then call `complete_task` with a short summary (what you changed, how you tested it, anything left open) and `tests_status` (`passed`, `failed` or `not_run`), and end your turn. Report the tests honestly: the user sees your report next to the diff. Do not merge, push or open a pull request yourself. If the user asks for changes, their note arrives as a message; make the changes and call `complete_task` again.
```

- [ ] **Step 1: Write the failing tests**

```go
// internal/install/plan4_test.go
func TestAllowToolsAddsSixthToAPlan3Install(t *testing.T) // allow holds the five -> complete_task appended once; a second run writes nothing
func TestUnhooksRemovesTheSixToolRules(t *testing.T)      // allow ["Bash(ls:*)","mcp__curia", the six] -> ["Bash(ls:*)","mcp__curia"]
func TestSkillExplainsFinishing(t *testing.T)             // the embedded file holds the paragraph above, the marker line, and Plan 3's two paragraphs
func TestInstallBindsNoPlan4Key(t *testing.T)             // the snippet for default keys holds none of the words "review", "restore", "serve", "ntfy"
// internal/install/plan2_test.go (changed): every count of five tool rules becomes six
// cmd/curia/install_test.go
func TestInstallAllowsSixToolsByDefault(t *testing.T)     // plain install -> the six strings, each once; stdout says six
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/install/ ./cmd/curia/ -run 'Tool|Install|Skill|Snippet' -v`. Expected: FAIL on the sixth name.
- [ ] **Step 3: Implement.** Existing tests that count five rules are updated, not deleted.
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/install/ ./cmd/curia/ -v`. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat: allow complete_task on install and explain finishing a task"`

**Done when:** the five tests pass and `internal/install/tmuxconf.go` is untouched by this task.

### Task 4: Board: review and done

**Model:** Opus (UI layout and keys).

**Files:**

- Modify: `internal/ui/board.go`, `internal/ui/board_view.go`
- Test: `internal/ui/board_test.go`

**Brief includes:** section 4.11 (Board), section 4.4.

**Interfaces:**

- Consumes: Plan 3's board (D13) and `task.Card` (D8); `model.TaskReview`, `model.TaskDone` (D2).
- Produces: no signature changes. The board shows cards whose `Status` is `review` in a fifth column (at 100 columns or more) or under a `review` heading (below). Cards whose `Status` is `done` are hidden until `D` is pressed, then shown in a sixth column at 120 columns or more and under a `done` heading otherwise; `D` again hides them, and the selection moves to the nearest visible card when its card is hidden. `r` (the ready toggle) and `n` (spawn) say why not on a `review` or `done` card: `T-42 is waiting for review: curia review T-42` and `T-42 is done`. Enter on a `review` card jumps to its pane as on any card. The help line gains `D done`. A pasted `D` is not a command.

- [ ] **Step 1: Write the failing tests** (the package's fake `BoardSource`)

```go
func TestBoardShowsReviewColumn(t *testing.T)        // width 120: five headed columns; a card with Status review is under "review"
func TestBoardHidesDoneUntilToggled(t *testing.T)    // a done card is absent; D -> present under "done"; D -> absent again
func TestBoardDoneColumnNeedsWidth(t *testing.T)     // D at width 110 -> list layout with six headings; at 130 -> six columns; no line wider than the width
func TestBoardSelectionLeavesHiddenCard(t *testing.T) // the selected card is done; D hides done -> another card is selected, never none while cards exist
func TestBoardNoSpawnOrReadyOnReviewOrDone(t *testing.T) // r and n on both -> no source call, the notices above
func TestBoardNarrowListsReview(t *testing.T)        // width 60: a "review" heading with its card
func TestBoardPastedDIsNotACommand(t *testing.T)
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/ui/ -run Board -v`. Expected: FAIL.
- [ ] **Step 3: Implement** in the two board files only.
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/ui/ -v`. Expected: PASS, every Plan 3 board test included (those that count four columns at width 120 now expect five and are updated, not deleted).
- [ ] **Step 5: Commit.** `git commit -m "feat: review and done on the task board"`

**Done when:** the seven tests pass.

## 9. Tasks 5 to 8 (Wave 2)

### Task 5: Review rules and the brief

**Model:** Sonnet (one pure package, exact wording).

**Files:**

- Create: `internal/task/review.go`
- Modify: `internal/task/brief.go`, `e2e/testdata/tasks/stub_argv.golden.txt` (by hand: the one changed line)
- Test: `internal/task/review_test.go`, `internal/task/brief_test.go`

**Brief includes:** sections 4.2, 4.3 (the argument rules), 4.6 (the message text and the pull-request step), and section 3's item 3.

**Interfaces:**

- Consumes: `model.Review`, `model.TestsStatus`, `model.MaxReviewNote` (Task 1); `model.Task`, `model.Decision`; `task.Problem`, `task.ValidID` (D8); `sanitize.Text`, `sanitize.Line`.
- Produces (all pure):

```go
const (
    MaxSummary     = 2000    // runes
    MaxTestsDetail = 200     // runes
    MaxPRBody      = 8 << 10 // bytes
)
func ValidReviewID(id string) bool // ^R-[1-9][0-9]{0,8}$

// Completion is complete_task's arguments.
type Completion struct {
    Summary     string
    Tests       model.TestsStatus
    TestsDetail string
}
// NormalizeCompletion cleans the texts (sanitize.Text and sanitize.Line with no cut, so that Validate can report a text that is too long).
func NormalizeCompletion(c Completion) Completion
// ValidateCompletion reports every problem of a normalised c, in the order summary, tests_status, tests_detail.
func ValidateCompletion(c Completion) []Problem

var ErrBadNote = errors.New("task: a review note must be 1 to 600 characters")
// NormalizeNote cleans a change request (sanitize.Text) and checks its length.
func NormalizeNote(note string) (string, error)
// NoteMessage is the message that carries a change request: "Review R-3 of T-42: changes requested. " + note.
func NoteMessage(reviewID, taskID, note string) string

// PRTitle is "T-42: " and the task's title on one line, every "@" removed, at most 120 runes.
func PRTitle(t model.Task) string
// PRBody is the pull-request description (the exact layout is below).
func PRBody(t model.Task, r model.Review, decided []model.Decision) string
```

`PRBody`, exact layout; the decisions line is left out when there are none; the detail part (` <detail>`) when it is empty:

```
Curia task T-42, review R-3.

Tests (the agent's report): passed. make check: 412 passed

Decisions on this task: D-4, D-9

Summary by the agent:

~~~~text
<the summary>
~~~~
```

Inside the fence every run of three or more `~` in the summary is replaced by `~~`, so the summary cannot close it. The summary is cut on a rune boundary so that the whole body is at most `MaxPRBody` bytes. Nothing outside the fence comes from the agent except the tests detail, which is one line with every `@` and every backtick removed.

The brief. `task.Brief` keeps its signature. Its last rule, which in Plan 3 reads

```
- When the task is finished, make sure everything is committed, then end your turn with a short summary: what you changed, how you tested it, and anything left open.
```

becomes exactly

```
- When the task is finished, make sure everything is committed, then call the curia MCP tool complete_task with a short summary (what you changed, how you tested it, anything left open) and the state of the tests, and end your turn. The user reviews the work; if they ask for changes, their note arrives as a message.
```

Nothing else in the brief changes. `e2e/testdata/tasks/stub_argv.golden.txt` holds the brief, so that one line is edited there by hand.

- [ ] **Step 1: Write the failing tests**

```go
// review_test.go
func TestValidReviewID(t *testing.T)            // "R-1", "R-42" ok; "R-0", "R-01", "r-1", "R-1 ", "R-1;x", "T-1", "" not
func TestValidateCompletionReportsEveryProblem(t *testing.T)
// empty summary; 2001 runes; tests "ok", ""; detail of 201 runes -> exactly the named field each; all three wrong -> three problems in order
func TestNormalizeCompletionCleans(t *testing.T) // summary with "\x1b[2J", U+202E and four blank lines -> none of the runes, one blank line; detail with a line break -> one line
func TestNormalizeNote(t *testing.T)            // "  fix \x1b[31mthis\n\n\n now " cleaned; "" and 601 runes -> ErrBadNote
func TestNoteMessage(t *testing.T)              // ("R-3","T-42","handle nil") == "Review R-3 of T-42: changes requested. handle nil"; with the longest ids and a 600-rune note it is at most 800 runes
func TestPRTitleHasNoMention(t *testing.T)      // title "Fix @alice's bug" -> "T-42: Fix alice's bug"; 200 runes -> 120; a title with a line break -> one line
func TestPRBodyLayout(t *testing.T)             // byte for byte against the layout above, with and without decisions and detail
func TestPRBodyFencesSummary(t *testing.T)
// summary "ok\n~~~~\n@everyone ![x](http://evil)\n~~~~~~~~" -> between the opening and closing fence lines no line of 4 or more "~"; the only "@" in the body is inside the fence
func TestPRBodyCapsAtEightKiB(t *testing.T)     // a 2000-rune summary of 4-byte runes -> len(body) <= MaxPRBody, valid UTF-8, the closing fence present
func TestPRBodyDetailCannotMention(t *testing.T) // detail "@bob `x`" -> the tests line holds neither "@" nor a backtick
// brief_test.go (changed)
func TestBriefWording(t *testing.T)             // solo and team, byte for byte, with the new last rule
func TestBriefNamesCompleteTask(t *testing.T)   // the brief holds "complete_task" once and no longer "end your turn with a short summary"
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/task/ -run 'Review|Completion|Note|PR|Brief' -v`. Expected: FAIL, undefined `task.ValidReviewID`.
- [ ] **Step 3: Implement,** then edit the golden file's one line by hand.
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/task/ ./cmd/curia/ ./e2e/ -v`. Expected: PASS, Plan 3's `TestTasksEndToEnd` and its launch tests included.
- [ ] **Step 5: Commit.** `git commit -m "feat: review rules, pull-request text and the complete_task brief"`

**Done when:** the twelve tests pass and Plan 3's acceptance test passes with the edited golden file.

### Task 6: Store: reviews and the session's server

**Model:** Opus (concurrent write transactions, a migration).

**Files:**

- Create: `internal/store/migrations/0005_reviews.sql` (section 4.1, exactly), `internal/store/reviews.go`
- Modify: `internal/store/store.go` (`upsert`, `selectSessions`, `scanSession`), `internal/store/store_test.go` (migration count)
- Test: `internal/store/reviews_test.go`

**Brief includes:** sections 4.1, 4.2, 4.3 (step 4), 4.4, 4.6 (what `CloseReview` writes), and section 3's items 10 and 11.

**Interfaces:**

- Consumes: `model.Review` and its enums, `model.Session.Socket` (Task 1); the package's `begin`, `formatTime`, `parseTime`, `selectDecisions`, `scanDecision`, `fail`; Plan 3's `tasks` table and `ErrNoTask` (D1, D4).
- Produces:

```go
var (
    ErrNoReview     = errors.New("store: no such review")
    ErrReviewClosed = errors.New("store: review is not pending")
)
type ReviewFilter struct {
    Statuses []model.ReviewStatus // empty: all
    TaskID   string               // empty: all
    Limit    int                  // 0: no limit
}
// CompleteTask records that r.TaskID's agent says it is done: it updates the task's pending review
// (summary, tests, session, updated_at) or opens a new pending one. r carries TaskID, SessionID, Summary,
// Tests, TestsDetail and CreatedAt (the time of the call). ErrNoTask: no such task, or it is stored dropped.
func (s *Store) CompleteTask(ctx context.Context, r model.Review) (stored model.Review, updated bool, err error)
func (s *Store) GetReview(ctx context.Context, id string) (model.Review, bool, error)
func (s *Store) ListReviews(ctx context.Context, f ReviewFilter) ([]model.Review, error) // by n, oldest first
// LatestReviews returns each task's newest review, by task id.
func (s *Store) LatestReviews(ctx context.Context) (map[string]model.Review, error)
// PendingReviewTasks returns the id of the pending review of each task that has one and is not stored dropped, by task id.
func (s *Store) PendingReviewTasks(ctx context.Context) (map[string]string, error)
// PendingReviews counts what PendingReviewTasks would return, in one statement (the badge).
func (s *Store) PendingReviews(ctx context.Context) (int, error)

// ReviewClose is what closing a pending review writes.
type ReviewClose struct {
    Status                 model.ReviewStatus // approved, merged, changes_requested or dropped
    How                    model.ApproveHow   // for approved and merged
    DiffStat, Head, PRURL  string
    Notes                  string             // for changes_requested
    Via                    model.AnsweredVia
}
// CloseReview closes pending review id as c says, in one write transaction. For approved and merged it
// also expires the task's open assume tickets and returns how many; for dropped it also stores the task
// as dropped. ErrNoReview; ErrReviewClosed when the review is not pending.
func (s *Store) CloseReview(ctx context.Context, id string, c ReviewClose, now time.Time) (r model.Review, expired int, err error)
// SetReviewNoteMessage records the message that carries a change request.
func (s *Store) SetReviewNoteMessage(ctx context.Context, id, messageID string) error
// TaskDecisions returns every ticket of the task, by n.
func (s *Store) TaskDecisions(ctx context.Context, taskID string) ([]model.Decision, error)
// TaskRun sums up the sessions ever linked to a task.
type TaskRun struct {
    Sessions, Teammates int
    Started             time.Time // the earliest started_at; zero when no session
    CostUSD             float64
    CostKnown           bool      // at least one linked session has a cost
}
func (s *Store) TaskRun(ctx context.Context, taskID string) (TaskRun, error)
```

Rules. Every write is one `BEGIN IMMEDIATE` transaction whose `UPDATE` names the status it expects, so a lost race changes nothing and returns the typed error. `CompleteTask` allocates `'R-' || (MAX(n) + 1)` inside its transaction. The expiry inside `CloseReview` is one statement: open assume tickets with that `task_id` become `expired` with `closed_at = now`; an empty task id expires nothing. `CloseReview` rejects a `Status` of `pending` with an error and changes nothing. `upsert` writes `tmux_socket` as `NULLIF(?, '')`; `selectSessions` and `scanSession` read it with `COALESCE`. `TaskRun` reads `sessions.cost_usd` (a `SUM` over the non-NULL values, `CostKnown` when any is non-NULL), `started_at` and `team_role` for sessions of any state with that `task_id`.

- [ ] **Step 1: Write the failing tests**

```go
func TestMigrationAddsReviewsAndPushes(t *testing.T)   // reviews, pushes exist; sessions has tmux_socket; 5 migrations recorded
func TestMigrationOnPlan3Database(t *testing.T)        // a database with 0001 to 0004 and one session, task and message (raw SQL), then store.Open -> all intact
func TestSessionSocketRoundTrip(t *testing.T)          // UpsertSession and UpdateSession keep Socket; "" is stored as NULL
func TestOldUpsertKeepsSocket(t *testing.T)            // run Plan 3's exact upsert statement (copied into the test) on a row with tmux_socket set -> still set
func TestCompleteTaskOpensThenUpdates(t *testing.T)    // first call: R-1 pending, updated false; second: the same id, new summary and updated_at, updated true, created_at kept
func TestCompleteTaskAfterCloseOpensNew(t *testing.T)  // after changes_requested, after approved and after merged -> R-2 pending
func TestCompleteTaskNeedsLiveTask(t *testing.T)       // unknown task, a dropped task, an empty task id -> ErrNoTask, no row
func TestConcurrentCompletesOneReview(t *testing.T)    // 5 handles, 20 goroutines for one task -> exactly one row; every call nil; exactly one updated == false
func TestConcurrentCompletesDistinctIDs(t *testing.T)  // 20 tasks at once -> R-1..R-20, all distinct
func TestReviewRoundTrip(t *testing.T)                 // every Review field survives close and get, times to the nanosecond
func TestCloseReviewTable(t *testing.T)                // one row per target status: the fields written, closed_at, closed_via; Status pending -> an error
func TestCloseReviewTwiceIsRefused(t *testing.T)       // the second call -> ErrReviewClosed, nothing changed; an unknown id -> ErrNoReview
func TestConcurrentClosesOneWins(t *testing.T)         // approve and drop race, 200 rounds on 5 handles -> exactly one nil, the other ErrReviewClosed
func TestApproveExpiresTaskAssumptions(t *testing.T)   // T-1: 2 open assume, 1 open blocking, 1 answered; T-2: 1 open assume -> approving T-1's review returns 2; only those two are expired; an expired ticket is not found by SearchDecisions
func TestChangesRequestedExpiresNothing(t *testing.T)
func TestDropSetsTaskDropped(t *testing.T)             // the task's stored status becomes dropped in the same transaction (a failing second statement, forced by a trigger the test adds, rolls both back)
func TestLatestReviewsIsNewestPerTask(t *testing.T)
func TestPendingReviewsSkipsDroppedTasks(t *testing.T) // 2 pending; one task stored dropped by raw SQL -> PendingReviews 1 and PendingReviewTasks has one key
func TestListReviewsFilters(t *testing.T)
func TestTaskDecisionsByTask(t *testing.T)             // tickets of T-1 in n order; a rule (no task) and T-2's are left out
func TestTaskRun(t *testing.T)                         // 1 lead and 2 teammates, one ended; cost_usd 1.5 and 2.0 set by raw SQL on two -> Sessions 3, Teammates 2, the earliest start, 3.5, known; no cost at all -> CostKnown false
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/store/ -run 'Review|Complete|Migration|Socket|TaskRun|TaskDecisions|Approve|Drop' -v`. Expected: FAIL, undefined `CompleteTask`.
- [ ] **Step 3: Implement.** One `selectReviews` column list and one `scanReview`. Concurrency tests open several handles and retry a call that failed with `SQLITE_BUSY` before its transaction ran, as Plan 2's do. `TestOpenIsIdempotent` and `TestConcurrentOpenOfFreshFile` expect 5 migrations.
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/store/ -v`. Expected: PASS, every earlier store test included.
- [ ] **Step 5: Commit.** `git commit -m "feat: reviews table, its transactions and the session's tmux server"`

**Done when:** the twenty-one tests pass under `-race`.

### Task 7: ntfy: rules and sender

**Model:** Opus (a network client, security-relevant wording).

**Files:**

- Create: `internal/notify/ntfy.go`
- Test: `internal/notify/ntfy_test.go` (the fixtures `internal/notify/testdata/ntfy/publish-accepted.json` and `publish-reply.json` are already there, committed by setup from probe N1)

**Brief includes:** section 4.7, section 4.13's push rows, section 2's "Secrets", and probe N1's recorded result.

**Interfaces:**

- Consumes: `model.Decision`, `model.Review`, `model.Task`; `config.ParseQuiet` (Task 1); `sanitize.Line`, `sanitize.Text`; the package's `repoPart`.
- Produces:

```go
const (
    MaxActions    = 3
    MaxPushText   = 1200 // runes of a message
    MaxPushTitle  = 80
    MaxPushLabel  = 30
    PublishLimit  = 5 * time.Second
)
// Action is one button: the phone POSTs Body to URL with "Authorization: Bearer <Token>".
type Action struct{ Label, URL, Token, Body string }
// Notification is one ntfy message, ready to publish.
type Notification struct {
    Topic, Title, Message string
    Priority              int
    Actions               []Action
}
// NotBefore is when a push made at now may go: now outside the quiet window (or with none), else the window's end, in now's location.
func NotBefore(now time.Time, quiet string) time.Time
// DecisionPush is the push for blocking ticket d. answerURL and token are "" for a push without buttons.
func DecisionPush(d model.Decision, topic, answerURL, token string) Notification
// ReviewPush is the push for a new review of t.
func ReviewPush(r model.Review, t model.Task, topic string) Notification
// NewToken returns a one-time token (43 characters of base64url) and its hash.
func NewToken(random io.Reader) (plain, hash string, err error)
// HashToken is the lowercase hex SHA-256 of plain.
func HashToken(plain string) string

// ErrRejected is wrapped when ntfy answered 4xx other than 429: sending the same message again cannot help.
var ErrRejected = errors.New("notify: ntfy rejected the message")
// Publisher sends notifications to one ntfy server.
type Publisher struct {
    URL         string       // the server's base URL
    AccessToken string       // "" for none
    Client      *http.Client // nil: a client that follows no redirect
}
func (p Publisher) Publish(ctx context.Context, n Notification) error
```

Rules: section 4.7's wording and request, exactly. `Publish` marshals the body with `encoding/json` (HTML escaping off), sets `Content-Type: application/json` and `User-Agent: curia`, bounds itself with `PublishLimit` when `ctx` has no earlier deadline, follows no redirect (`CheckRedirect` returns `http.ErrUseLastResponse`, and a 3xx is an error), reads at most 4 KiB of the reply and discards it, returns nil for 2xx, an error wrapping `ErrRejected` for 4xx other than 429, and a plain error otherwise. No error text contains the URL's path, a token or the body: it names the status code or the transport error's kind only. `DecisionPush` adds no action when `answerURL` or `token` is empty or the ticket is irreversible. A zero `Priority` is sent as 3.

- [ ] **Step 1: Write the failing tests** (a local `httptest.Server` stands in for ntfy)

```go
func TestNotBeforeTable(t *testing.T)
// quiet "22:00-07:00" in a fixed location: 21:59 -> unchanged; 22:00 -> 07:00 next day; 03:30 -> 07:00 same day; 06:59:59 -> 07:00; 07:00 -> unchanged;
// quiet "09:00-17:00": 08:59 unchanged, 12:00 -> 17:00, 17:00 unchanged; quiet "" and "junk" -> unchanged
func TestDecisionPushWording(t *testing.T)       // a 3-option blocking ticket in repo /src/api-server -> title "D-117 api-server: decision needed", the message lines of section 4.7, priority 4
func TestDecisionPushButtons(t *testing.T)       // recommended "B" of A, B, C -> labels in order B, A, C; each body is {"decision":"D-117","key":"<k>"}; one token on all three; URL == answerURL
func TestDecisionPushFourOptions(t *testing.T)   // 4 options -> 3 buttons, the recommended first, and the "More options: curia decide D-117." line
func TestDecisionPushIrreversibleHasNoButtons(t *testing.T) // and the "Irreversible: answer with curia decide D-117." line
func TestDecisionPushWithoutServe(t *testing.T)  // answerURL "" -> no actions, the message unchanged
func TestPushTextIsClean(t *testing.T)           // question, labels and summary holding ESC, BEL, U+202E, 5000 runes -> no control rune anywhere; message <= 1200 runes, title <= 80, labels <= 30
func TestReviewPushWording(t *testing.T)         // title "R-3 T-42: ready for review"; the three parts; priority 3; no actions
func TestNewTokenShapeAndHash(t *testing.T)      // 43 chars of [A-Za-z0-9_-]; hash == HashToken(plain) and is 64 hex chars; two calls differ; a failing reader -> error
func TestPublishMatchesCapturedRequest(t *testing.T) // the JSON the server receives equals testdata/ntfy/publish-accepted.json after the token is replaced by TOKEN
func TestPublishSetsBearerOnlyWhenGiven(t *testing.T)
func TestPublishStatusTable(t *testing.T)        // 200 -> nil; 400, 401, 403, 413 -> errors.Is ErrRejected; 429, 500, 503 -> error, not ErrRejected; a closed server -> error, not ErrRejected
func TestPublishDoesNotFollowRedirect(t *testing.T) // the server answers 307 to a second server -> error; the second server saw no request
func TestPublishTimesOut(t *testing.T)           // a server that never answers and a context of 50 ms -> error within 1 s
func TestPublishErrorLeaksNothing(t *testing.T)  // for every failing row above: the error text holds neither the token, the access token, the topic nor the message
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/notify/ -run 'NotBefore|Push|Token|Publish' -v`. Expected: FAIL, undefined `notify.NotBefore`.
- [ ] **Step 3: Implement.** If probe N1 recorded another request shape, the orchestrator has amended section 4.7 and this task before dispatch; the captured fixture is the reference.
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/notify/ -v`. Expected: PASS, Plan 2's notify tests included.
- [ ] **Step 5: Commit.** `git commit -m "feat: ntfy pushes: quiet hours, wording, one-time tokens, publisher"`

**Done when:** the fifteen tests pass and no test opened a connection off loopback.

### Task 8: Review view

**Model:** Opus (UI; key handling is where Plan 2 needed four fix rounds).

**Files:**

- Create: `internal/ui/review.go` (model), `internal/ui/review_view.go` (view)
- Test: `internal/ui/review_test.go`

**Brief includes:** sections 4.5 and 4.6 (the three actions' prompts and outcome lines), section 3's item 6, and from `AGENTS.md` the entries on Bubble Tea's draw timer and on Rulings 63, 66 to 68, 88 and 93.

**Interfaces:**

- Consumes: `model.Review`, `model.Task`, `model.Decision`, `model.ApproveHow`, `model.MaxReviewNote` (Task 1); `sanitize`; the package's `dwell` (250 ms), `quietPeriod` (750 ms), `answeredQuiet` (2 s), `fit`, `truncate`, `Age`.
- Produces:

```go
// ReviewRow is one waiting review in the queue.
type ReviewRow struct {
    Review model.Review
    Task   model.Task
}
// ReviewDetail is everything the view shows about one review. Every string in it is untrusted.
type ReviewDetail struct {
    Review      model.Review
    Task        model.Task
    DiffStat    string           // "" when there is none to show
    DiffNote    string           // a warning or the reason there is no diff; "" when none
    Assumptions []model.Decision // open assume tickets of the task
    Blocking    []model.Decision // open blocking tickets of the task
    Decided     []model.Decision
    Sessions    int
    Teammates   int
    Duration    time.Duration
    CostUSD     float64
    CostKnown   bool
    Approve     model.ApproveHow // what the a key will do
}
// ReviewSource is where the view reads and acts. The command's implementation runs git.
type ReviewSource interface {
    Queue(ctx context.Context) ([]ReviewRow, error)                          // pending reviews, oldest first; no git
    Detail(ctx context.Context, reviewID string) (ReviewDetail, error)       // may take seconds
    Approve(ctx context.Context, reviewID string) (line string, err error)   // may take minutes
    RequestChanges(ctx context.Context, reviewID, note string) (line string, err error)
    Drop(ctx context.Context, reviewID string) (line string, err error)
    DiffCmd(ctx context.Context, reviewID string) (*exec.Cmd, error)         // the configured diff tool
}
type ReviewOptions struct {
    TaskID   string // not empty: only this task's pending review
    Embedded bool   // inside the dashboard: closing sends ReviewClosedMsg instead of quitting
}
type ReviewClosedMsg struct{}
type ReviewModel struct{ /* unexported */ }
func NewReview(src ReviewSource, now func() time.Time, opts ReviewOptions) ReviewModel // a tea.Model
```

Behaviour.

- **Reading.** `Queue` once a second; `Detail` for the shown review when it first appears, when its `UpdatedAt` changes, and every 5 seconds. The shown review is kept by id across refreshes. An empty queue reads `no review is waiting`; with `TaskID` and no pending review, `T-42 has no review waiting`. A source error is shown on the status line and the last good data stays.
- **Layout.** Section 4.5, top to bottom, with `review 1 of 3` at the top right. `]` and `[` (and `n`, `p`) move to the next and previous review; `j` `k` and the arrows scroll.
- **`o`** runs `DiffCmd` through `tea.ExecProcess`; an error from it or from the tool is shown.
- **`a`** opens the prompt `merge <branch> into <base>? y confirms, any other key cancels`, or `push <branch> and open a pull request? y confirms, any other key cancels`. With open assumptions, a red line above it: `2 unconfirmed assumptions (D-4, D-9) will expire.`
- **`c`** opens a note input capped at `model.MaxReviewNote` runes; Enter with a non-blank note sends; Esc cancels and keeps nothing.
- **`x`** opens `drop T-42? its agent is not stopped. y confirms, any other key cancels`.
- **`q`, Esc** close (outside a prompt or the input).
- **While an action runs** the status line reads `working…` and every key is ignored, `q`, Esc and Ctrl+C included; the view closes only after the action returned. Its line or its error is then shown.

Key rules, all with the injected clock:

1. Pasted text is never a command, in any state.
2. `a`, `c`, `x` and `o` act only when the shown review has been on screen, unchanged (same id and `UpdatedAt`), for at least `dwell`; earlier they show `too soon; pause, then press again` and restart the dwell.
3. `y` confirms only at least `dwell` after its prompt opened, and only if the review is still the one, unchanged, that the prompt opened on. If the review changed or left the queue while a prompt or the note input is open, it is canceled with `the review changed; read it again`.
4. In the note input, an Enter within `dwell` of the input opening is ignored, and an Enter within `quietPeriod` of the Enter before it is a repeat and is ignored with the same notice.
5. After an action returned, every key but `q` and Esc is ignored for `answeredQuiet`, and each ignored key restarts that window.

- [ ] **Step 1: Write the failing tests** (a fake `ReviewSource` recording calls; an injected clock)

```go
func TestReviewShowsDetailInOrder(t *testing.T)        // the head line, red assumption and blocking lines, summary, tests, diff, decisions, the sessions line, in section 4.5's order
func TestReviewAssumptionsAreRedAndFirst(t *testing.T)
func TestReviewCostNotRecorded(t *testing.T)           // CostKnown false -> "cost not recorded"; true -> "$4.12"
func TestReviewDiffNoteShown(t *testing.T)
func TestReviewEmptyQueueAndNoReviewForTask(t *testing.T)
func TestReviewMovesBetweenReviews(t *testing.T)       // ] and [ ; Detail called for the newly shown id
func TestReviewKeepsShownReviewAcrossRefresh(t *testing.T) // the queue reorders -> the same review is shown
func TestApproveNeedsConfirmationAfterDwell(t *testing.T)  // a then y in one read -> no Approve; a, wait, y -> Approve(id) once; a then any other key -> no call
func TestApprovePromptNamesAssumptions(t *testing.T)
func TestApprovePromptSaysMergeOrPR(t *testing.T)      // Detail.Approve merge and pr -> the two prompts
func TestActionKeysWaitForDwell(t *testing.T)          // a, c, x, o 100 ms after the review appeared -> no prompt, the notice
func TestPromptCanceledWhenReviewChanges(t *testing.T) // the review's UpdatedAt changes while the approve prompt is open -> y does nothing, the notice; the same for the note input and the drop prompt
func TestRequestChangesSendsNote(t *testing.T)         // c, type, Enter after a pause -> RequestChanges(id, note) once; a blank note -> no call
func TestNoteInputCap(t *testing.T)                    // 700 runes typed -> the source gets 600
func TestHeldEnterDoesNotSendNote(t *testing.T)        // c, then Enters at 100 ms; type; Enters 300 ms apart -> at most one RequestChanges, sent by the first Enter that came after a pause
func TestDropNeedsConfirmation(t *testing.T)
func TestReviewPasteIsNotACommand(t *testing.T)        // pasted "a", "y", "x", "c" and a pasted "y" inside a prompt -> no call
func TestKeysIgnoredWhileActionRuns(t *testing.T)      // Approve blocks: q, Esc, Ctrl+C, a -> ignored; when it returns its line is shown
func TestQuietAfterAction(t *testing.T)                // a second a within 2 s of an action's return -> ignored; after 2 s without keys it acts
func TestActionErrorShown(t *testing.T)
func TestOpenDiffRunsSourceCommand(t *testing.T)       // o -> DiffCmd(id) once; its error shown
func TestReviewEmbeddedCloseSendsMessage(t *testing.T)
func TestReviewTextCannotDrawEscapes(t *testing.T)     // summary, diff stat, note, title, question, branch holding ESC, BEL, U+202E -> none in View
func TestReviewNarrowWidth(t *testing.T)               // width 40 -> no line wider than 40 cells
func TestReviewSourceErrorKeepsLastDetail(t *testing.T)
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/ui/ -run 'Review|Approve|Prompt|Note|Drop|Quiet|Action|OpenDiff|HeldEnter' -v`. Expected: FAIL, undefined `ui.NewReview`.
- [ ] **Step 3: Implement** in the two new files only. The clock is the injected `now`; refreshes are `tea.Tick`.
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/ui/ -v`. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat: review view with guarded approve, change request and drop"`

**Done when:** the twenty-five tests pass.

## 10. Tasks 9 to 13 (Wave 3)

### Task 9: Store: pushes and the restore claim

**Model:** Opus (claims raced by several processes).

**Files:**

- Create: `internal/store/pushes.go`
- Modify: `internal/store/housekeeping.go`
- Test: `internal/store/pushes_test.go`, `internal/store/housekeeping_test.go`

**Brief includes:** sections 4.1 (`pushes`), 4.7 (Outbox), 4.8 (step 3), 4.10 (step 2), and section 3's items 4 and 5.

**Interfaces:**

- Consumes: `model.Push` and its enums (Task 1); Task 6's migration; the package's `meta` table pattern (`ClaimNotifySlot`).
- Produces:

```go
const (
    PushReclaim     = 2 * time.Minute // a sending row claimed longer ago is claimed again
    PushMaxAttempts = 5
    PushTokenTTL    = 24 * time.Hour
)
var ErrBadToken = errors.New("store: unknown, spent, expired or mismatched token")

// EnqueuePushes adds one pending push per device for (kind, ref), due at notBefore, and returns how many
// rows it added; a row that exists for (kind, ref, device) is left as it is.
func (s *Store) EnqueuePushes(ctx context.Context, kind model.PushKind, ref string, devices []string, notBefore, now time.Time) (int, error)
// PushesDue reports whether ClaimPush would find a row at now. It writes nothing.
func (s *Store) PushesDue(ctx context.Context, now time.Time) (bool, error)
// ClaimPush claims the oldest due push (section 4.7, step 2) and stores tokenHash on it ("" for none).
func (s *Store) ClaimPush(ctx context.Context, tokenHash string, now time.Time) (model.Push, bool, error)
// FinishPush records the result of the claimed push n: sent (sets sent_at), dropped, failed, or pending again
// with not_before = retryAt. errText is stored cut to 200 bytes. It changes only a row that is sending.
func (s *Store) FinishPush(ctx context.Context, n int64, status model.PushStatus, errText string, retryAt, now time.Time) error
// UsePushToken spends the one-time token whose hash is tokenHash for decisionID (section 4.8, step 3).
func (s *Store) UsePushToken(ctx context.Context, tokenHash, decisionID string, now time.Time) (model.Push, error)

// ClaimRestore claims the right to run curia restore at now, one holder at a time; a claim older than hold is taken over.
func (s *Store) ClaimRestore(ctx context.Context, now time.Time, hold time.Duration) (bool, error)
// ReleaseRestore gives the claim up.
func (s *Store) ReleaseRestore(ctx context.Context) error
```

Rules. Each write is one `BEGIN IMMEDIATE` transaction or one statement. `ClaimPush` picks by `n` among rows matching `pushes_due`'s predicate with `not_before <= now` (pending) or `claimed_at <= now − PushReclaim` (sending), and in the same statement sets `status = 'sending'`, `attempts = attempts + 1`, `claimed_at = now`, `token_hash`. `FinishPush` with `sent` keeps the hash; with any other status it clears it, so a token of a push that was not sent can never be spent. `UsePushToken` succeeds only for an empty-proof hash (a `""` hash is `ErrBadToken` without a query), `kind = 'decision'`, `ref = decisionID`, `status = 'sent'`, `used_at IS NULL` and `sent_at > now − PushTokenTTL`, and sets `used_at = now`; every other case is `ErrBadToken`, with no way for the caller to tell them apart. `ClaimRestore` is `ClaimNotifySlot`'s statement on the key `restore_claim_at`; `ReleaseRestore` deletes the key.

- [ ] **Step 1: Write the failing tests**

```go
func TestEnqueueOnePerDeviceOnce(t *testing.T)      // two devices -> 2 rows; again -> 0 added, rows unchanged; a third device later -> 1
func TestPushRoundTrip(t *testing.T)                // every Push field, times to the nanosecond
func TestPushesDueRespectsNotBefore(t *testing.T)   // due in 1 h -> false; at that time -> true; PushesDue writes nothing (data_version unchanged)
func TestClaimPushOldestDueFirst(t *testing.T)
func TestClaimPushReclaimsDeadSender(t *testing.T)  // sending for 1 min -> not claimed; for 3 min -> claimed, attempts 2, a new hash replaces the old
func TestPushClaimedExactlyOnce(t *testing.T)       // 5 handles, 20 goroutines, 10 due rows -> each row claimed once
func TestFinishPushTable(t *testing.T)              // sent keeps the hash and sets sent_at; dropped, failed and pending clear it; pending sets not_before; a row that is not sending is not changed
func TestFinishPushCutsErrorText(t *testing.T)
func TestUsePushTokenOnce(t *testing.T)             // the right hash and decision -> the push, used_at set; again -> ErrBadToken
func TestUsePushTokenRefusals(t *testing.T)         // "" hash; an unknown hash; another decision id; a review push; a push still sending, failed or dropped; sent 25 h ago -> ErrBadToken each, nothing written
func TestUsePushTokenRace(t *testing.T)             // 5 handles, 20 goroutines, one token -> exactly one nil
func TestClaimRestoreOneHolder(t *testing.T)        // first true, second false; after hold has passed true; after ReleaseRestore true at once
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/store/ -run 'Push|Enqueue|Token|Restore' -v`. Expected: FAIL, undefined `EnqueuePushes`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/store/ -v`. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat: push outbox, one-time tokens and the restore claim"`

**Done when:** the twelve tests pass under `-race`.

### Task 10: Fleet and attention: review-ready, other servers

**Model:** Opus (the re-check inside `Reconcile`'s transaction; three packages).

**Files:**

- Modify: `internal/fleet/fleet.go`, `internal/attention/attention.go`, `internal/install/tmuxconf.go` (the value of `markerFormat` only)
- Test: `internal/fleet/fleet_test.go`, `internal/attention/attention_test.go`, `internal/install/tmux_real_test.go`

**Brief includes:** sections 4.9 (third item) and 4.11 (Rank, Window marker), section 2's hook budget and "tmux installer rulings", and section 3's item 8.

**Interfaces:**

- Consumes: `model.Session.Socket`, `.ReviewPending`, `.TaskID` (Task 1, D2); `tmux.Pane.Socket` (Task 2); the store's socket column and `store.PendingReviewTasks` (Task 6); `markerFormat` and the snippet as merged (D18).
- Produces:

```go
// WithReviews returns a copy of sessions in which ReviewPending is set on every session that is neither
// ended nor stale and whose TaskID is a key of pending (task id to review id). The input is not changed.
func WithReviews(sessions []model.Session, pending map[string]string) []model.Session
```

  - `Rank`'s groups become, in order: needs_decision, waiting, **review-ready**, idle-unseen, working, idle-seen, stale, unknown. Review-ready is a session whose state is `idle` and whose `ReviewPending` is set, seen or not. `Next` jumps to it (the jump limit stays idle-unseen, which now comes after it). `Units` follows, because it ranks with the same `group`.
  - `attention.Marker(s model.Session) string` returns `"blue"` when it would return `""`, `s.ReviewPending` is set, the state is neither `ended` nor `stale`, and `s.SeenAt` is before `s.UpdatedAt`. `attention.WindowMarker` returns red when any session is red, else blue when any is blue. `attention.Apply` keeps its signature: after listing the window's sessions it reads `st.PendingReviewTasks` and marks them with `WithReviews` before it computes the marker; when that read fails it applies the marker without reviews and returns the read's error wrapped. It runs no other new statement.
  - `internal/install/tmuxconf.go`: `markerFormat` becomes exactly `#{?#{==:#{@curia_marker},red},#[fg=red] ●,#{?#{==:#{@curia_marker},blue},#[fg=blue] ●,}}`. No other line of the snippet changes: no format is set or appended beyond what is there, no key, no option.
  - `Reconcile` keeps its signature. The live server's socket is the first non-empty `Socket` among the panes it is given. A session whose `Socket` is not empty, when that server socket is not empty and differs, is never marked stale: not when listed and not in the re-check inside `markStale`'s update. A session or a server with no known socket behaves as before.

- [ ] **Step 1: Write the failing tests**

```go
func TestWithReviewsMarksLiveSessionsOfPendingTasks(t *testing.T) // idle and working sessions of T-1 marked; ended and stale ones not; T-2 (no review) and a session with no task not
func TestWithReviewsDoesNotMutateInput(t *testing.T)
func TestRankPutsReviewReadyAfterWaiting(t *testing.T)   // needs_decision, waiting, review-ready (seen), idle-unseen, working, idle-seen -> that order
func TestReviewPendingWorkingSessionRanksAsWorking(t *testing.T) // the agent was sent back to work: not review-ready
func TestNextJumpsToReviewReady(t *testing.T)            // only a seen, idle, review-ready session and a working one -> Next returns the first
func TestReconcileLeavesOtherServerAlone(t *testing.T)   // session socket /tmp/a, panes report /tmp/b and do not list its pane -> still idle; no window returned
func TestReconcileSameServerStillMarksStale(t *testing.T) // equal sockets, pane gone -> stale
func TestReconcileUnknownSocketBehavesAsBefore(t *testing.T) // session socket "" or panes without a socket -> Plan 2's behaviour
func TestReconcileRecheckHonoursSocket(t *testing.T)     // a new row of the table in recheck_test.go's TestMarkStaleRechecksTheStoredRow: the row gains another server's socket between the list pass and the update -> not marked stale
// internal/attention/attention_test.go
func TestMarkerBlueForUnseenReviewPending(t *testing.T)  // ReviewPending, idle, SeenAt before UpdatedAt -> "blue"; seen -> ""; ReviewPending and waiting unseen -> "red"; ended or stale -> ""
func TestWindowMarkerRedBeatsBlue(t *testing.T)
func TestApplySetsBlueFromPendingReview(t *testing.T)    // a real store with a task, a pending review and an unseen linked session; tmux.Fake -> SetWindowOption(window, "@curia_marker", "blue"); after MarkSeen -> ""; after CloseReview -> ""
func TestApplyWithoutReviewsTableStillAppliesRed(t *testing.T) // the reviews table dropped by the test: a waiting unseen session still gets red, and Apply returns an error
// internal/install/tmux_real_test.go
func TestMarkerFormatDrawsBlueAndRed(t *testing.T)       // real tmux, the snippet sourced: a window with @curia_marker blue expands window-status-format to end "#[fg=blue] ●", red to "#[fg=red] ●", unset to no dot
func TestMarkerUpgradeNeedsOnlyTheOption(t *testing.T)   // source the snippet built with the old red-only markerFormat (the literal in the test), set a user's own window-status-format before it, then source the new snippet -> the format string is byte for byte what it was after the first source; blue now draws
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/fleet/ ./internal/attention/ ./internal/install/ -run 'Review|Reconcile|Next|Rank|Marker|Apply' -v`. Expected: FAIL, undefined `fleet.WithReviews`.
- [ ] **Step 3: Implement.** The existing rank, marker and snippet tests keep their assertions except those that compare the literal `markerFormat`, which get the new value.
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/fleet/ ./internal/attention/ ./internal/install/ ./cmd/curia/ -v`. Expected: PASS, Plan 3's `Units` tests and the hook latency tests included.
- [ ] **Step 5: Commit.** `git commit -m "feat: rank and mark review-ready sessions, leave other tmux servers alone"`

**Done when:** the fifteen tests pass and the diff of `tmuxconf.go` is the one constant.

### Task 11: Task status from reviews

**Model:** Sonnet (one table in a pure function and its one caller).

**Files:**

- Modify: `internal/task/status.go`, `cmd/curia/task.go` (`taskCards`)
- Test: `internal/task/status_test.go`, `cmd/curia/task_test.go`

**Brief includes:** section 4.4.

**Interfaces:**

- Consumes: `model.Review`, `model.ReviewStatus` (Task 1); `store.LatestReviews` (Task 6); Plan 3's `Derive`, `Cards`, `Card`, `taskCards` (D8, D11).
- Produces (replacing Plan 3's two signatures):

```go
// Derive is the status shown for t (section 4.4). latest is the status of t's newest review, "" when it has none.
func Derive(t model.Task, sessions []model.Session, openBlocking int, latest model.ReviewStatus) model.TaskStatus
// Cards derives every task's card. latest holds each task's newest review, by task id.
// Order: by status as the board's columns run (backlog, ready, running, needs_decision, review, done), then by n.
func Cards(tasks []model.Task, sessions []model.Session, openBlocking map[string]int, latest map[string]model.Review) []Card
```

  - `task.Card` gains `Review model.Review` (the newest review; zero when none).
  - `taskCards` keeps its signature and also reads `st.LatestReviews`. `curia task ls` therefore shows `review` and `done` (its `--status` flag has accepted both since Plan 3); its JSON keys do not change.

- [ ] **Step 1: Write the failing tests**

```go
// internal/task/status_test.go
func TestDeriveWithReviews(t *testing.T)
// pending + a working session -> review; pending + no session -> review; approved -> done; merged -> done;
// changes_requested + idle session -> running; changes_requested + no session -> the stored status;
// dropped review + session -> running; stored dropped + pending review -> dropped; "" -> every row of Plan 3's TestDeriveTable unchanged
func TestCardsCarryNewestReviewAndOrder(t *testing.T) // a review card sorts after needs_decision and before done; Card.Review is the map's entry
// cmd/curia/task_test.go
func TestTaskLsShowsReviewThenDone(t *testing.T)     // CompleteTask -> "review"; CloseReview(merged) -> "done"; a later CompleteTask -> "review" again; no other command ran in between
func TestTaskLsStatusFilterReviewAndDone(t *testing.T)
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/task/ ./cmd/curia/ -run 'Derive|Cards|TaskLs' -v`. Expected: FAIL, too few arguments to `Derive`.
- [ ] **Step 3: Implement.** Every existing call and test of `Derive` and `Cards` gains the new argument (`""` or `nil`); a `git grep -n 'task\.\(Derive\|Cards\)('` must show no caller left with the old shape.
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/task/ ./internal/ui/ ./internal/route/ ./cmd/curia/ ./e2e/ -v`. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat: derive review and done from a task's newest review"`

**Done when:** the four tests pass and Plan 3's task, board and acceptance tests pass.

### Task 12: MCP: `complete_task`

**Model:** Opus (the tool surface; two packages).

**Files:**

- Create: `internal/mcp/complete.go`
- Modify: `internal/mcp/tools.go` (`Deps`, the list in `Tools`), `e2e/testdata/decisions/tools_list.golden.json` (by hand: the sixth tool)
- Test: `internal/mcp/complete_test.go`, `internal/mcp/tools_test.go`, `cmd/curia/mcp_test.go`

**Brief includes:** section 4.3, section 2's "Only I decide", and section 3's item 1.

**Interfaces:**

- Consumes: `task.Completion`, `NormalizeCompletion`, `ValidateCompletion` (Task 5); `store.CompleteTask`, `store.ErrNoTask` (Task 6); `mcp.Caller` with `TaskID` and `TeamRole` (D12); the package's `wrap`, `decodeArgs`, `invalidArguments`, `internal`, `Problem`, `ToolError`.
- Produces:
  - `mcp.Deps` gains `Reviewed func(ctx context.Context, r model.Review, pane string)`: "called after a new review was opened (not when a pending one was updated); may be nil".
  - `mcp.Tools` returns six tools; the sixth, after `check_messages`, has the name, description and schema of section 4.3 and the handler in `complete.go`.
  - The handler follows section 4.3's five steps in order and returns its result object; `review`, `task` and `instruction` are always present and `updated` is a boolean. A store error other than `ErrNoTask` is the fixed `internal` error.
  - `mcp.Instructions` is not changed, so no client golden file of an earlier acceptance test changes.

- [ ] **Step 1: Write the failing tests**

```go
// internal/mcp/tools_test.go
func TestToolSurfaceIsExactlySix(t *testing.T)          // names, in order; replaces the test of five; no tool name holds "approve", "merge", "review_", "answer", "drop" or "restore"
// internal/mcp/complete_test.go
func TestCompleteCreatesReview(t *testing.T)            // Caller{SessionID, TaskID T-1} -> result review R-1, task T-1, updated false, the instruction of section 4.3; the row is pending with the caller's session
func TestCompleteAgainUpdates(t *testing.T)             // a second call -> the same id, updated true; Reviewed called once in total
func TestCompleteCallsReviewedWithPane(t *testing.T)    // Reviewed gets the stored review and Caller.Pane; a nil Reviewed is fine
func TestCompleteInvalidListsAllProblems(t *testing.T)  // {} -> summary and tests_status; {"summary":7} -> one problem on summary; tests_status "green" -> its problem; no row
func TestCompleteCleansText(t *testing.T)               // a summary with "\x1b[2J" and U+202E is stored without them
func TestCompleteTeammateRefused(t *testing.T)          // TeamRole teammate -> isError, code ask_lead, section 4.3's sentence, no row; the same for a malformed call from a teammate
func TestCompleteWithoutTaskRefused(t *testing.T)       // Caller with no session; with a session and no TaskID -> code no_task, no row
func TestCompleteUnknownOrDroppedTask(t *testing.T)     // -> code no_task naming the task
func TestCompleteStoreErrorIsInternal(t *testing.T)     // a closed store -> code internal with the fixed message; the store's text is not in the result
func TestCompleteIgnoresExtraArguments(t *testing.T)    // {"summary":"x","tests_status":"passed","task":"T-9","status":"approved"} from T-1's session -> a pending review of T-1
// cmd/curia/mcp_test.go
func TestMCPOverPipesListsSixTools(t *testing.T)        // replaces the test of five
func TestCompleteTaskOverPipes(t *testing.T)            // a session row with task T-1 in TMUX_PANE: tools/call complete_task over real pipes -> R-1; curia's store shows it
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/mcp/ ./cmd/curia/ -run 'Tool|Complete' -v`. Expected: FAIL.
- [ ] **Step 3: Implement,** then edit `tools_list.golden.json` by hand.
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/mcp/ ./cmd/curia/ ./e2e/ -v`. Expected: PASS, the earlier acceptance tests included.
- [ ] **Step 5: Commit.** `git commit -m "feat: complete_task tool opens a review"`

**Done when:** the thirteen tests pass. `cmd/curia/mcp.go` is not edited here (Task 15 wires `Reviewed`).

### Task 13: Hook: record the server

**Model:** Opus (the hook path and its budget).

**Files:**

- Modify: `internal/hook/run.go`, `cmd/curia/hook.go`
- Test: `internal/hook/hook_test.go`, `cmd/curia/hook_test.go`

**Brief includes:** section 4.9 (second item), section 2's hook budget and output, and section 3's item 9.

**Interfaces:**

- Consumes: `tmux.Pane.Socket` (Task 2); `model.Session.Socket` and the store column (Tasks 1, 6); Plan 3's `hook.Env` (D14).
- Produces: `hook.Env.PaneInfo` becomes `func(pane string) (tmuxSession, window, socket string)`: "the name of the tmux session, the id of the window and the socket path of the server that hold pane". `startInfo` gains `socket`. In `apply`: on a SessionStart that starts the session, `Socket` is set from `start` (also to empty: a session restarted outside tmux has none); for a session first seen mid-run it fills only an empty `Socket`, as the other start fields do; a compaction's SessionStart for a live session changes nothing. `hookEnv` in `cmd/curia/hook.go` returns `p.Socket` from the `PaneInfo` call it already makes. No process, statement, file read or environment read is added.

- [ ] **Step 1: Write the failing tests**

```go
// internal/hook/hook_test.go
func TestSessionStartStoresSocket(t *testing.T)        // Env.PaneInfo returns ("api", "@7", "/tmp/tmux-1/default") -> stored
func TestSessionStartOutsideTmuxHasNoSocket(t *testing.T) // Env.Pane "" -> Socket "", PaneInfo never called
func TestResumeInNewServerReplacesSocket(t *testing.T) // a stale row with socket A; SessionStart (source resume) with PaneInfo returning socket B and a new pane -> pane and socket both replaced, state idle
func TestSocketFilledMidRunOnlyWhenEmpty(t *testing.T)
func TestCompactionKeepsSocket(t *testing.T)
func TestSessionStartStillOneTmuxCall(t *testing.T)    // a counting Env: one PaneInfo and one Git call for a SessionStart; none for the next five events
// cmd/curia/hook_test.go
func TestHookStoresSocketFromFakeTmux(t *testing.T)    // useFakeTmux with a pane whose Socket is set -> the session row has it; the fake's Calls hold exactly one PaneInfo
func TestHookLatencyUnchanged(t *testing.T)            // TestHookLatency's method and threshold (p95 < 20 ms, 200 samples, no retry) with a socket-bearing pane; p50 and p95 reported
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/hook/ ./cmd/curia/ -run 'Socket|Server|TmuxCall|Latency' -v`. Expected: FAIL, `Env.PaneInfo` has two results.
- [ ] **Step 3: Implement.** Every existing test's `Env.PaneInfo` gains the third result.
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/hook/ ./cmd/curia/ -v`. Expected: PASS, every earlier latency test included and unchanged.
- [ ] **Step 5: Commit.** `git commit -m "feat: record which tmux server a session's pane is on"`

**Done when:** the eight tests pass, the hook's stdout and exit rules are untouched (the existing `TestStdoutOnlyOnBlockedStop…` tests pass unchanged), and the PR states p50 and p95.

## 11. Tasks 14 to 18 (Wave 4)

### Task 14: Deliver: never type across servers

**Model:** Opus (the typing path).

**Files:**

- Modify: `internal/deliver/deliver.go`, `internal/deliver/message.go`
- Test: `internal/deliver/deliver_test.go`, `internal/deliver/message_test.go`

**Brief includes:** section 4.9 (fourth item), section 3's item 8, and from `AGENTS.md` "The typing path, final order of conditions" and the entries on Rulings 54, 58, 81, 85 and 86.

**Interfaces:**

- Consumes: `tmux.Pane.Socket` (Task 2); `model.Session.Socket` as the claims return it (`store.TypingClaim.Session`, `store.MessageClaim.Session`); `store.RequeueDelivery`, `store.RequeueMessage`; Plan 3's `deliver.Message` (D6).
- Produces: no signature changes. One refusal is added to the typing path of both `deliver.Answer` and `deliver.Message`, between the claim (step 3 of the order in `AGENTS.md`) and the capture (step 4): when the claimed session's `Socket` is not empty and the socket of the panes listed in step 2 (the first non-empty `Pane.Socket`) is not empty and differs, nothing is captured and nothing is typed, the claim is requeued exactly as a pane refusal is, and the note is `the agent is on another tmux server (<name>); nothing typed; tell the agent to call check_decision D-7` (or `check_messages`), where `<name>` is the last path element of the session's socket through `sanitize.Line(…, 40)`. An unknown socket on either side changes nothing. No existing condition is removed or reordered, and the refusal lives in the code both paths share (the pane check); if they share none, it is written once and called from both.

- [ ] **Step 1: Write the failing tests** (`tmux.Fake` with `Panes` carrying `Socket`, the idle screen fixture, a real store)

```go
// deliver_test.go
func TestAnswerNotTypedAcrossServers(t *testing.T)   // session socket /tmp/a/default, panes report /tmp/b/other, the same pane id exists there and shows the idle prompt -> Calls hold no CapturePane and no SendKeys; the answer is queued; the note names "default" and check_decision
func TestAnswerTypedOnSameServer(t *testing.T)       // equal sockets -> typed as before
func TestAnswerUnknownSocketTypedAsBefore(t *testing.T) // session "" or panes without a socket -> typed as before
func TestCrossServerNoteIsClean(t *testing.T)        // a stored socket whose last element holds ESC and 200 runes -> no control rune in the note, the name cut
func TestCrossServerRefusalLeavesAnswerCollectable(t *testing.T) // after the refusal the Stop hook's claim and check_decision still get the answer, once
// message_test.go
func TestMessageNotTypedAcrossServers(t *testing.T)  // the same for deliver.Message: queued, the note names check_messages
func TestMessageTypedOnSameServer(t *testing.T)
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/deliver/ -run 'Server|Socket' -v`. Expected: FAIL (the line is typed).
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/deliver/ -v`. Expected: PASS, every Plan 2 and Plan 3 delivery test included and unchanged, the real-tmux ones not skipped.
- [ ] **Step 5: Commit.** `git commit -m "fix: never type an answer or message into another tmux server"`

**Done when:** the seven tests pass.

### Task 15: Push outbox and MCP wiring

**Model:** Opus (goroutines in a long-lived process, the network).

**Files:**

- Create: `cmd/curia/push.go`
- Modify: `cmd/curia/mcp.go`
- Test: `cmd/curia/push_test.go`

**Brief includes:** section 4.7, section 2's "Network is opt-in" and "Secrets", and section 3's item 5.

**Interfaces:**

- Consumes: `attention.Apply` with the blue marker (Task 10); `notify.NotBefore`, `DecisionPush`, `ReviewPush`, `NewToken`, `Publisher`, `ErrRejected` (Task 7); `store.EnqueuePushes`, `PushesDue`, `ClaimPush`, `FinishPush`, `PushMaxAttempts` (Task 9); `store.GetDecision`, `GetReview`, `GetTask`; `mcp.Deps.Reviewed` (Task 12); `config.Ntfy`, `config.Serve` (Task 1); `mcpNotify`, `mcpServe`, `clock`.
- Produces, in package `main`:

```go
// pushEnqueue queues one push per configured device for a new blocking ticket or a new review.
// It does nothing, and touches no table, when ntfy is not enabled.
func pushEnqueue(ctx context.Context, st *store.Store, cfg config.Config, kind model.PushKind, ref string, now time.Time) error
// pushPublisher is the publisher for cfg: its URL, CURIA_NTFY_TOKEN when that is usable, no redirects.
func pushPublisher(cfg config.Config) notify.Publisher
// pushFlush sends every push that is due, one at a time, until none is due or ctx ends, and returns how many it sent.
func pushFlush(ctx context.Context, st *store.Store, cfg config.Config, pub notify.Publisher, now func() time.Time) (int, error)
// pushLoop flushes when kick fires and every pushEvery, until ctx ends. It never returns an error: failures are logged.
func pushLoop(ctx context.Context, st *store.Store, cfg config.Config, logger *slog.Logger, kick <-chan struct{})
var pushEvery = 60 * time.Second // tests shorten it
const pushBackoff = 30 * time.Second
const pushBackoffMax = 15 * time.Minute
```

Rules: section 4.7's four steps, exactly. `pushFlush` generates a token (`notify.NewToken(crypto/rand.Reader)`) only for a decision push when `cfg.Serve.PublicURL` is not empty, passes its hash to `ClaimPush` and its plain form only to `notify.DecisionPush` with the answer URL `<public_url>/v1/answer`; for every other push the hash is `""`. A device name that is no longer in `cfg.Ntfy.Devices`: `dropped`. Log lines name the push's `n`, kind, ref, device and outcome, never a token, a topic or message text. The publisher's URL is `cfg.Ntfy.URL` and nothing else: no flag, argument or environment variable can set it (gosec's G704 reports a request whose URL derives from the command line). `CURIA_NTFY_TOKEN` is used only when it is at most 512 bytes with no rune below `0x20`; otherwise it is ignored and one warning is logged.

`cmd/curia/mcp.go`:

- When `cfg.Ntfy.Enabled()`, `mcpServe` starts `pushLoop` in a goroutine with a buffered kick channel of size 1 and a context it cancels when `Serve` returns; it then waits at most 3 seconds for the loop to end. When ntfy is not enabled no goroutine is started and no publisher is built.
- `mcpNotify` calls `pushEnqueue(…, model.PushDecision, d.ID, …)` for the blocking ticket it is given, before and independently of the bell's rate-limit slot (a ticket that rings no bell is still pushed), then kicks the loop without blocking.
- `mcp.Deps.Reviewed` is set to a function that re-applies the window marker of the calling session (`st.GetSession(r.SessionID)`, then `attention.Apply` on its `TmuxWindow`, bounded by `subprocessTimeout`; skipped for a session with no window), then calls `pushEnqueue(…, model.PushReview, r.ID, …)` and kicks the loop. Errors are logged, never returned to the tool. The marker is applied whether or not ntfy is configured.

- [ ] **Step 1: Write the failing tests** (a local `httptest.Server` as ntfy; real store; an injected clock)

```go
func TestNoNtfyConfiguredNoNetwork(t *testing.T)     // default config: pushEnqueue adds no row; mcpServe over pipes with a blocking ticket and a complete_task -> a listener the test owns saw no connection and no pushes row exists
func TestEnqueueOnePerDeviceWithQuietHours(t *testing.T) // 2 devices, quiet covering now -> 2 rows due at the window's end
func TestFlushSendsDuePushes(t *testing.T)           // one decision push, 2 devices -> the server got 2 requests with the 2 topics; both rows sent; returns 2
func TestFlushHoldsUntilQuietEnds(t *testing.T)      // not due -> 0 requests; the clock moved past not_before -> sent
func TestFlushButtonsNeedPublicURL(t *testing.T)     // public_url "" -> no actions and token_hash ""; set -> 3 actions whose token hashes to the row's token_hash
func TestFlushTokenIsNotStoredInClear(t *testing.T)  // the plain token from the request appears in no column of the row and in no log line
func TestFlushDropsAnsweredTicket(t *testing.T)      // the ticket is answered before the flush -> dropped, no request; the same for a withdrawn ticket and a review that is no longer pending
func TestFlushDropsRemovedDevice(t *testing.T)
func TestFlushRetriesThenFails(t *testing.T)         // the server answers 503: attempt 1 -> pending due in 30 s; 2 -> 60 s; … attempt 5 -> failed; never more than 5 requests
func TestFlushRejectedIsNotRetried(t *testing.T)     // 403 -> failed after one request
func TestFlushRetryRespectsCap(t *testing.T)         // no not_before is more than 15 minutes ahead
func TestTwoFlushersSendOnce(t *testing.T)           // two goroutines on two store handles, 10 due pushes -> the server got exactly 10 requests
func TestFlushReviewPush(t *testing.T)               // a review push -> title "R-1 T-1: ready for review", no actions
func TestPushLoopKickAndTick(t *testing.T)           // pushEvery 20 ms: a kick flushes at once; a push that becomes due later is sent by a tick; cancelling ctx ends the loop within 1 s
func TestMCPBlockingTicketIsPushed(t *testing.T)     // mcpServe over pipes, ntfy configured: request_decision (blocking) -> within 2 s the server got its push; an assume ticket -> none
func TestMCPNewReviewIsPushedOnce(t *testing.T)      // complete_task twice -> one review push
func TestMCPNewReviewMarksWindowBlue(t *testing.T)   // useFakeTmux, ntfy not configured: complete_task from an unseen session with a window -> the fake got SetWindowOption <window> @curia_marker blue
func TestMCPPushDoesNotDelayTheTool(t *testing.T)    // a server that takes 2 s to answer: the tool's reply arrives in under 500 ms
func TestMCPStopsLoopOnEOF(t *testing.T)             // stdin closed -> mcpServe returns within 4 s even while the server hangs
func TestBadNtfyTokenIgnored(t *testing.T)           // CURIA_NTFY_TOKEN holding a line break -> no Authorization header, one warning in the log without the value
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./cmd/curia/ -run 'Push|Flush|Enqueue|Ntfy|MCPBlocking|MCPNew|MCPStops' -v`. Expected: FAIL, undefined `pushEnqueue`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./cmd/curia/ -v`. Expected: PASS, every Plan 2 and Plan 3 MCP test included.
- [ ] **Step 5: Commit.** `git commit -m "feat: push blocking tickets and reviews through an outbox"`

**Done when:** the twenty tests pass under `-race` and no test connected off loopback.

### Task 16: `curia review`

**Model:** Opus (git and `gh` at a process boundary; judgment about what is safe to merge).

**Files:**

- Create: `cmd/curia/review.go` (the command, the flag forms, the `ui.ReviewSource`), `cmd/curia/review_git.go` (the diff stat, merge, push and pull request, the diff tool)
- Test: `cmd/curia/review_test.go`, `cmd/curia/review_git_test.go`

**Brief includes:** sections 4.5, 4.6, 4.13, section 2's "git and gh", and section 3's items 1 to 3.

**Interfaces:**

- Consumes: `ui.NewReview`, `ReviewSource`, `ReviewRow`, `ReviewDetail`, `ReviewOptions` (Task 8); `store.GetReview`, `ListReviews`, `PendingReviewTasks`, `CloseReview`, `ReviewClose`, `SetReviewNoteMessage`, `TaskDecisions`, `TaskRun`, `ErrNoReview`, `ErrReviewClosed` (Task 6); `task.ValidReviewID`, `NormalizeNote`, `NoteMessage`, `PRTitle`, `PRBody` (Task 5); Plan 3's `store.GetTask`, `TaskSessions`, `CreateMessage` (D4, D5), `deliver.Message` (D6), `task.ValidID`, `task.ValidBranch` (D8), `worktreeDirty`, `refExists`, `sendOutcomeLine` (D11); `runGit`, `gitEnv`, `refuseInsideAgent`, `decideVia`, `runProgram`, `applyMarkers`, `loadConfig`, `openStore`, `openLog`, `newTmuxClient`, `clock`, `jsonTime`.
- Produces, in package `main` (every name starts with `review` and none with `reviews`):

```go
// review_git.go
// reviewDiffStat is section 4.5's diff stat for t, cleaned and capped, and the warning line that goes with it ("" when none).
func reviewDiffStat(ctx context.Context, t model.Task, width int) (stat, note string)
// reviewCheck runs steps 1 to 5 of section 4.6's approve order and returns the diff stat and the branch's commit.
func reviewCheck(ctx context.Context, st *store.Store, t model.Task) (stat, head string, err error)
// reviewMerge is step 6: it merges t's branch into its base in t.Repo, and aborts a merge that did not complete.
func reviewMerge(ctx context.Context, t model.Task) error
// reviewOpenPR is step 7: push, then gh pr create; url is "" when gh printed none.
func reviewOpenPR(ctx context.Context, t model.Task, r model.Review, decided []model.Decision, remote string) (url string, err error)
// reviewGH runs gh in dir and returns its stdout (at most 64 KiB). Tests replace it.
var reviewGH = func(ctx context.Context, dir string, args ...string) (stdout []byte, err error)
// reviewDiffLookPath finds a diff tool. Tests replace it.
var reviewDiffLookPath = exec.LookPath
// reviewDiffCmd is the command of section 4.6's diff table for tool.
func reviewDiffCmd(t model.Task, tool string) (*exec.Cmd, error)
const (
    reviewMergeLimit = 2 * time.Minute
    reviewPushLimit  = 2 * time.Minute
    reviewGHLimit    = time.Minute
)

// review.go
// newReviewSource returns the ui.ReviewSource of curia review. Task 20 reuses it for the dashboard.
func newReviewSource(st *store.Store, tm tmux.Client, cfg config.Config, logger *slog.Logger, via model.AnsweredVia) ui.ReviewSource
// reviewApprove, reviewRequestChanges and reviewDrop are the three actions of section 4.6; each returns its outcome line.
func reviewApprove(ctx context.Context, st *store.Store, cfg config.Config, reviewID string, how model.ApproveHow, via model.AnsweredVia, now time.Time) (string, error)
func reviewRequestChanges(ctx context.Context, st *store.Store, tm tmux.Client, logger *slog.Logger, reviewID, note string, via model.AnsweredVia, now time.Time) (string, error)
func reviewDrop(ctx context.Context, st *store.Store, reviewID string, via model.AnsweredVia, now time.Time) (string, error)
// reviewResolve turns a task id or a review id into the pending review it names.
func reviewResolve(ctx context.Context, st *store.Store, arg string) (model.Review, error)
```

The command. `curia review [<T-id>|<R-id>] [--approve [--merge|--pr]] [--request-changes --note <text>|-] [--drop] [--json]`:

- It calls `refuseInsideAgent` first, before the config or the store is opened; help and usage errors are exempt (Ruling 69).
- With no action flag: on a terminal it runs `ui.NewReview` (all pending reviews, or the one named) through `runProgram` with the alt screen; without a terminal it exits 1 and says to use `curia reviews ls` or an action flag. `--json` instead prints the named review's detail as one JSON object (the review's columns, `task`, `diff_stat`, `diff_note`, `open_assumptions` and `open_blocking` as id arrays, `decided` as an id array, `sessions`, `teammates`, `duration_seconds`, `cost_usd` or null) and acts on nothing.
- With an action flag an id is required; exactly one of `--approve`, `--request-changes`, `--drop`; `--merge` and `--pr` only with `--approve` and not both; `--note` only with and required by `--request-changes`, read from stdin (at most 64 KiB) when it is `-`. The flag form records `closed_via = cli`; the view records `decideVia()`.
- An id that names no pending review: exit 1 with `T-42 has no review waiting` or `R-3 is merged` (the status it has).

Rules: sections 4.5 and 4.6, exactly, in their order. Besides:

- Every git call goes through `runGit` with its own limit. `<base>` and `<branch>` are checked with `task.ValidBranch` before any git call and are never the first argument after a subcommand's fixed flags; the range is passed as the single argument `<base>...<branch>`.
- `reviewMerge` never passes `--force`, `-X`, `--no-verify` or `--allow-unrelated-histories`, never runs `checkout`, `reset`, `stash` or `clean`, and after any failed merge leaves `git status --porcelain` in `<repo>` exactly as it was before.
- `reviewOpenPR` never passes `--force` to push and never pushes the base. `reviewGH`'s real implementation finds `gh` with `exec.LookPath`, runs it with `exec.CommandContext` (no shell), `cmd.Dir = dir`, `cmd.WaitDelay` set, and returns stderr's first line, cleaned, in its error. Its argv is `["pr","create","--base",base,"--head",branch,"--title",title,"--body",body]`.
- `reviewRequestChanges` writes in this order: `CloseReview`, `CreateMessage`, `SetReviewNoteMessage`, `deliver.Message`. A failure after `CloseReview` is reported as `R-3: changes requested; the note is saved but could not be sent: <error>` with exit 1.
- `reviewDiffCmd` returns an error naming the tool when `reviewDiffLookPath` does not find it. The command's environment is `gitEnv()`.
- After each of the three actions the window markers of the task's live linked sessions are re-applied (`applyMarkers` from `common.go` over their distinct windows), so a blue dot goes when the review is closed; a marker failure is logged, never returned.
- `Detail` in the source builds section 4.5's fields: the open tickets and the decided ones from `TaskDecisions`, the run from `TaskRun`, `Approve` from the config, the diff stat at width 100.

- [ ] **Step 1: Write the failing tests** (real git repositories under `t.TempDir()` made with `gitEnv`: a main checkout on `main`, a linked worktree on `curia/T-1-x` with one commit, a local bare repository as `origin`; `useFakeTmux`; a recording `reviewGH`)

```go
// review_git_test.go
func TestDiffStatAgainstBase(t *testing.T)           // two changed files -> their names and the "2 files changed" line; the base moved on since the branch left it -> only the branch's changes (three dots)
func TestDiffStatNotes(t *testing.T)                 // a dirty worktree -> the uncommitted-changes warning; a missing branch; no commits beyond base -> their notes
func TestDiffStatIsCappedAndClean(t *testing.T)      // 400 changed files -> 200 lines and "… N more lines"; a file name holding ESC -> no control rune
func TestDiffStatWorksWithoutWorktree(t *testing.T)  // the worktree removed (a paused task) -> still the stat, from the main repository
func TestMergeHappyPath(t *testing.T)                // -> a merge commit on main whose second parent is the branch's head; the worktree untouched
func TestMergeRefusals(t *testing.T)
// one row each: no branch on the task; a branch that does not resolve; base "-x"; a linked session working; one waiting; a dirty worktree;
// no commits beyond base; main checkout on another branch; main checkout detached -> the line of section 4.6, HEAD of main unchanged, review pending
func TestMergeConflictAborts(t *testing.T)           // both sides changed one line -> error ending "; nothing was merged"; no MERGE_HEAD; status --porcelain empty; HEAD unchanged
func TestMergeHookFailureAborts(t *testing.T)        // a commit-msg hook that exits 1 -> the same; and a pre-merge-commit hook that exits 1
func TestMergeKeepsMyLocalChanges(t *testing.T)      // an unrelated modified file in the main checkout -> merged, the file still modified; a modified file the branch also changes -> refused by git, my change intact, HEAD unchanged
func TestNeverForces(t *testing.T)                   // a wrapper git on PATH that records argv: across the happy path, every refusal and the PR path no argv holds "--force", "-f", "--no-verify", "reset", "checkout", "stash" or "clean"
func TestPRPushesAndCallsGh(t *testing.T)            // -> origin has the branch at the head; reviewGH called once in the repo with the argv above; title == task.PRTitle, body == task.PRBody; the URL it printed is returned
func TestPRArgvIsDirect(t *testing.T)                // a summary and title full of ";", "$(x)", backticks and line breaks -> they arrive as single argv elements, byte for byte after task.PRBody / PRTitle
func TestPRRejectsOddURL(t *testing.T)               // gh prints "see http://x", "javascript:alert(1)", 300 chars -> url ""
func TestPRGhMissingOrFailing(t *testing.T)          // LookPath fails -> the line naming gh and --merge, nothing pushed; gh exits 1 after the push -> error with its first stderr line cleaned, review pending
func TestDiffCmdTable(t *testing.T)                  // git, delta, lazygit (worktree present and absent), nvim -> the argv of section 4.6; a tool that is not found -> an error naming it
// review_test.go
func TestReviewRefusesInsideAgent(t *testing.T)      // CLAUDECODE=1: the view, --approve, --request-changes, --drop and --json all exit 1; the store file is not created
func TestReviewApproveMerges(t *testing.T)           // --approve -> the merged line with the short head; review merged with diff_stat, head, approved_how merge, closed_via cli
func TestReviewApproveExpiresAssumptions(t *testing.T) // 2 open assume tickets on the task -> the line says 2 expired; both expired
func TestReviewApprovePRFlagAndConfig(t *testing.T)  // --pr, and review.approve = "pr" without a flag -> approved with pr_url; --merge overrides the config
func TestReviewApproveFailureStaysPending(t *testing.T)
func TestReviewRequestChangesDelivers(t *testing.T)  // an idle live session, the idle screen fixture -> review changes_requested with the note; a message of kind instruction, routed_by address, target the session; SendKeys with the typed line; the line "R-1: changes requested; M-1 delivered (send_keys)"; note_message M-1
func TestReviewRequestChangesQueuesForWorkingAgent(t *testing.T) // -> "…; M-1 queued: …", no SendKeys
func TestReviewRequestChangesTargetsLeadNotTeammate(t *testing.T)
func TestReviewRequestChangesWithoutLiveAgent(t *testing.T) // the review's session ended -> the message is queued for that session id; no session at all -> "the note is saved but T-1 has no agent session to send it to", no message row
func TestReviewNoteFromStdinAndBounds(t *testing.T)  // --note - reads stdin; empty and 601 runes -> exit 1, review still pending
func TestReviewDrop(t *testing.T)                    // -> review dropped, task stored dropped, the line of section 4.6
func TestReviewActionReappliesMarker(t *testing.T)   // after --approve, --request-changes and --drop the fake got SetWindowOption for the agent's window with a value that is not "blue"
func TestReviewFlagErrors(t *testing.T)              // two actions; --pr without --approve; --merge with --pr; --note without --request-changes; an action without an id; "T-01"; "R-0" -> usage errors, nothing written
func TestReviewResolveByTaskAndByReview(t *testing.T) // T-1 and R-1 name the same pending review; a closed one -> "R-1 is merged"; a task with none -> "T-1 has no review waiting"
func TestReviewJSON(t *testing.T)                    // keys and null handling, written out in the test; nothing is changed by it
func TestReviewWithoutTerminalNeedsAnAction(t *testing.T)
func TestReviewOutputIsSanitised(t *testing.T)       // a title and a git error holding ESC -> none on stdout or stderr
func TestReviewSourceDetail(t *testing.T)            // open assume, open blocking and decided tickets land in their three lists; sessions, teammates, duration and cost from TaskRun
func TestReviewSourceQueueRunsNoGit(t *testing.T)    // the recording git wrapper saw no call during Queue
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./cmd/curia/ -run 'Review|Merge|DiffStat|PR|DiffCmd|NeverForces' -v`. Expected: FAIL, unknown command `review`.
- [ ] **Step 3: Implement.** In `review_test.go` a package-level default replaces `reviewGH` and `reviewDiffLookPath` with stubs that fail any test which has not set its own.
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./cmd/curia/ -v`. Expected: PASS. No test runs the real `gh`, `lazygit`, `delta` or `nvim`.
- [ ] **Step 5: Commit.** `git commit -m "feat: curia review approves, requests changes or drops finished work"`

**Done when:** the thirty-four tests pass.

### Task 17: `curia reviews`

**Model:** Sonnet (a thin read-only command over finished store calls).

**Files:**

- Create: `cmd/curia/reviews.go`, `cmd/curia/testdata/reviews.golden.json` (hand-written)
- Test: `cmd/curia/reviews_test.go`

**Brief includes:** section 4.11 (`curia reviews ls`), section 4.1 (the `reviews` columns).

**Interfaces:**

- Consumes: `store.ListReviews`, `ReviewFilter` (Task 6); Plan 3's `store.ListTasks` (D4); `ui.Cell`, `ui.Age`, `sanitize.Line`, `jsonTime`, `escapeUnprintable`, `loadConfig`, `openStore`, `clock`.
- Produces: `curia reviews ls [--status pending|all|pending|approved|changes_requested|merged|dropped] [--json]` (default `pending`), in package `main` with every name starting `reviews`. Table: `ID`, `AGE` (since `created_at`), `TASK` (`T-42` and the title cut to 30 runes), `STATUS` (`pending`, `approved (pr)`, `merged`, `changes requested`, `dropped`), `TESTS`, `SUMMARY` (first line, 60 runes); every cell through `ui.Cell(sanitize.Line(…))`. JSON: an array (`[]` when empty) of objects whose keys are `id`, `task_id`, `session_id`, `summary`, `tests_status`, `tests_detail`, `status`, `approved_how`, `diff_stat`, `head`, `pr_url`, `my_notes`, `note_message`, `closed_via`, `created_at`, `updated_at`, `closed_at`, times as RFC 3339 UTC or null. It reads only and is allowed inside an agent session.

- [ ] **Step 1: Write the failing tests**

```go
func TestReviewsLsDefaultIsPending(t *testing.T)   // one review of each status -> only the pending one
func TestReviewsLsStatusFilterAndAll(t *testing.T) // each status alone; all -> five, by n; an unknown status -> a usage error naming the valid ones
func TestReviewsLsJSONGolden(t *testing.T)         // three reviews (pending, merged with head and diff_stat, changes_requested with a note and M-1) -> reviews.golden.json after every non-null time becomes "<time>"
func TestReviewsLsEmptyIsEmptyArray(t *testing.T)
func TestReviewsLsTableStatusWords(t *testing.T)   // "approved (pr)", "changes requested"
func TestReviewsLsTableIsSanitised(t *testing.T)   // a summary and a title stored by raw SQL holding ESC and U+202E -> no such rune on stdout
func TestReviewsLsAllowedInsideAgent(t *testing.T) // CLAUDECODE=1 -> exit 0
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./cmd/curia/ -run ReviewsLs -v`. Expected: FAIL, unknown command `reviews`.
- [ ] **Step 3: Implement.** The golden file is written by hand from section 4.1's columns, never from the program's output.
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./cmd/curia/ -v`. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat: curia reviews ls"`

**Done when:** the seven tests pass.

### Task 18: `curia restore`

**Model:** Opus (tmux and `claude` at a process boundary; judgment about what is safe to relaunch).

**Files:**

- Create: `cmd/curia/restore.go`
- Test: `cmd/curia/restore_test.go`

**Brief includes:** sections 4.9, 4.10, 4.13 (the stored-session row), section 3's items 7 and 8, and the results of probes R1 to R3.

**Interfaces:**

- Consumes: `tmux.Pane.Socket` (Task 2); `model.Session.Socket` (Tasks 1, 6); `store.ClaimRestore`, `ReleaseRestore` (Task 9); `config.Restore` (Task 1); Plan 3's `tmux.WindowSpec` and `NewWindow` (D9), `task.SessionName` (D8), `store.GetTask`, `ListTasks`, `UpdateTask`, `CreateMessage` (D4, D5), `deliver.Message` (D6), `sendOutcomeLine` (D11), the `launch` command's arguments (D10); `store.ListSessions`, `GetSession`, `UpdateSession`; `executablePath`, `refuseInsideAgent`, `loadConfig`, `openStore`, `openLog`, `newTmuxClient`, `clock`.
- Produces, in package `main` (every name starts with `restore`):

```go
type restoreAction string // "restore", or a skip reason of section 4.10's table, as printed
// restoreRow is one line of the plan.
type restoreRow struct {
    Session    model.Session
    Task       model.Task  // zero when the session has no usable task
    Linked     bool        // the task exists and is not dropped
    LastActive time.Time   // the transcript's modification time; zero when there is none
    Action     restoreAction
    Detail     string      // what follows "skip:" for the user, already clean
}
// restorePlan classifies every session that is not ended (section 4.10, step 3). It reads the file system
// (directories and transcripts) and nothing else. Sessions classed "live" are left out.
func restorePlan(sessions []model.Session, live []tmux.Pane, tasks map[string]model.Task, now time.Time, since time.Duration) []restoreRow
// restoreSpec is the window for one row whose Action is "restore" (section 4.10, step 5).
func restoreSpec(row restoreRow, bin, claude string) (tmux.WindowSpec, error)
var (
    restoreLookPath   = exec.LookPath // finds claude; tests replace it
    restoreSleep      = time.Sleep    // tests replace it and clock
    restoreIsTerminal = func() bool   // stdin and stdout are terminals; tests replace it
)
const (
    restoreWait     = 30 * time.Second
    restorePoll     = 250 * time.Millisecond
    restoreMax      = 32
    restoreHold     = 2 * time.Minute
    restoreLeadText = "Curia restored this session after a restart. Your teammates did not survive it: respawn your team from the shared task list, then continue."
)
```

Rules: section 4.10, exactly, in its order. Besides:

- `restore` refuses inside an agent session before anything is opened.
- A session is "live" only when its pane id is among `live`, its `StartedAt` is not before that pane's `ServerStart`, and its socket is unknown or equal to the listed panes' socket.
- `restoreSpec` builds every field from validated values only: the session id matched against the UUID pattern, the task id by `task.ValidID`, the model by `task.ValidModel` (a stored model that fails is left out), names by the replacement rule, `claude` an absolute path from `restoreLookPath`, the directory an absolute cleaned path that `os.Stat` showed to be a directory. It returns an error rather than a spec that `NewWindow` would refuse.
- The transcript check uses `os.Lstat`: a regular file only. The directory check uses `os.Stat`.
- The team notice is a message with kind `instruction`, sender `user`, `routed_by` `address`, status `queued`, the lead's session as target; it is stored after the lead is seen live, then `deliver.Message` is called once and its outcome line printed.
- Session rows are changed only through `store.UpdateSession`, whose function re-checks the row: a teammate or a superseded old row is set `ended` only if it is still not live in a listed pane.
- When `claude` is not on `PATH` and at least one unlinked session is to be restored, those rows fail with `claude is not on PATH`; linked ones still go through `curia launch`.

- [ ] **Step 1: Write the failing tests** (real store; `useFakeTmux`, whose `NewWindow` appends a pane; transcripts as real files with `os.Chtimes`; worktrees as real directories; a fixed clock)

```go
func TestRestoreRefusesInsideAgent(t *testing.T)
func TestRestoreNeedsATmuxServer(t *testing.T)        // ListPanes fails (the TestMain guard's unreachable tmux) -> exit 1 with the line of step 1; no claim taken
func TestPlanClassifiesEverySession(t *testing.T)
// one session per row of section 4.10's table, in one store: live; other server; outside tmux; teammate; agent "codex"; id "abc;kill-server";
// task paused; directory removed; transcript missing; transcript a directory; transcript 3 days old; two candidates of one task; one good candidate
// -> each row's Action and Detail; the live one absent; ended sessions absent
func TestPlanPaneReusedAfterServerRestartIsNotLive(t *testing.T) // the pane id is listed but the session started before ServerStart -> a candidate
func TestPlanSinceWindow(t *testing.T)                // a transcript 30 h old: default -> too old; --since 48h -> restore
func TestPlanCapsAtThirtyTwo(t *testing.T)            // 40 candidates -> 32 restore (the most recently active), 8 "over the limit; run again"
func TestRestoreDryRunChangesNothing(t *testing.T)    // prints the table; no NewWindow, no row changed, the claim released
func TestRestoreNeedsConfirmation(t *testing.T)       // no --yes, no terminal -> exit 1 naming --yes; a terminal and "n" or an empty line -> nothing launched; "y" -> launched
func TestRestoreSecondRunRefused(t *testing.T)        // the claim held by another -> exit 1 "another curia restore is running"; after it ends a run works
func TestRestoreLinkedTaskGoesThroughLaunch(t *testing.T)
// -> one NewWindow: session == task.SessionName(repo), name T-1, dir the worktree, env CURIA_TASK_ID=T-1,
// command [bin launch T-1 --resume <id>]; with agent_model and team -> … --model opus --team; the task row has the new pane and launched_at
func TestRestoreUnlinkedSessionRunsClaudeResume(t *testing.T) // command [<abs claude> --resume <id>], dir the cwd, no env; tmux session "my_repo_x" from a stored "my.repo:x"; window from the repo base
func TestRestoreNeverPassesStoredTextToTmux(t *testing.T)     // stored tmux session name, repo and cwd full of ";", "$(x)" and line breaks -> every NewWindow field matches Plan 3's patterns; a cwd that is not a directory is skipped
func TestRestoreNeverAddsPermissionFlags(t *testing.T)        // no command argument starts with "--dangerously" or "--permission"
func TestRestoreWaitsForSessionToReport(t *testing.T) // a goroutine upserts the session live in the new pane after 3 fake sleeps -> "restored …"; exit 0
func TestRestoreReportsSilentSession(t *testing.T)    // nothing reports within restoreWait -> "… did not report within 30s; look at api-server:T-1"; exit 1
func TestRestoreNewSessionIDEndsOldRow(t *testing.T)  // another live session appears in the new pane -> the old row is ended and the line says restored
func TestRestoreNewWindowFailureContinues(t *testing.T) // NewWindowErr for the first -> its line; the second is still tried; exit 1
func TestRestoreTeamLead(t *testing.T)                // a lead with 2 teammate rows -> only the lead gets a window; both teammates ended; one message with restoreLeadText for the lead; deliver.Message's outcome line printed
func TestRestoreLeavesLiveTeammateAlone(t *testing.T) // a teammate whose pane is listed -> not ended
func TestRestoreWithoutClaude(t *testing.T)           // restoreLookPath fails: an unlinked row fails with the line; a linked row still launches
func TestRestoreOtherServerUntouched(t *testing.T)    // a session of another socket: skipped, its row unchanged
func TestRestoreOutputIsSanitised(t *testing.T)       // a repo and branch holding ESC -> none on stdout
func TestRestoreInRealTmux(t *testing.T)              // private server, a stub script as claude that records its argv and cwd: an unlinked session gets a window in the right tmux session whose pane ran [stub --resume <id>] in the cwd
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./cmd/curia/ -run 'Restore|Plan' -v`. Expected: FAIL, unknown command `restore`.
- [ ] **Step 3: Implement.** `restorePlan` is a list of small per-class checks so it stays under the complexity limits. In the test file a package-level default replaces `restoreLookPath` with a stub that fails any test which has not set its own.
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./cmd/curia/ -v`. Expected: PASS, the real-tmux test not skipped where tmux is installed. No test runs the real `claude`.
- [ ] **Step 5: Commit.** `git commit -m "feat: curia restore relaunches lost agent sessions"`

**Done when:** the twenty-three tests pass.

## 12. Tasks 19 to 22 (Waves 5 and 6)

### Task 19: `curia serve`

**Model:** Opus (a network surface, security-sensitive).

**Files:**

- Create: `cmd/curia/serve.go`
- Test: `cmd/curia/serve_test.go`

**Brief includes:** sections 4.8, 4.13 (the HTTP request row), section 2's "Network is opt-in", "Secrets" and "Only I decide", and section 3's items 1 and 4.

**Interfaces:**

- Consumes: `store.UsePushToken`, `ErrBadToken` (Task 9); `notify.HashToken` (Task 7); `pushLoop` (Task 15); `deliver.Answer`; `store.GetDecision`, `ErrNotOpen`, `ErrNotFound`; `decision.ValidID`, `decision.ErrBadAnswer`; `model.AnsweredNtfy`, `config.Serve`, `config.TailscaleAddr` (Task 1); `refuseInsideAgent`, `loadConfig`, `openStore`, `openLog`, `newTmuxClient`, `clock`.
- Produces, in package `main` (every name starts with `serve`):

```go
// serveDeps is what the handler runs on.
type serveDeps struct {
    Store *store.Store
    Tmux  tmux.Client
    Log   *slog.Logger
    Now   func() time.Time
}
// serveHandler is the endpoint's whole HTTP surface (section 4.8).
func serveHandler(d serveDeps) http.Handler
// serveListenAddr is the address to listen on: flag, else cfg, else the first Tailscale address in addrs with port 8787.
// It returns an error for an address that is neither loopback nor Tailscale, and when none is found.
func serveListenAddr(flag string, cfg config.Serve, addrs []net.Addr) (string, error)
// serveInterfaceAddrs lists this machine's addresses. Tests replace it.
var serveInterfaceAddrs = net.InterfaceAddrs
const (
    servePort          = 8787
    serveMaxBody       = 1 << 10
    serveMaxFailures   = 10 // failed token checks a minute before 429
    serveShutdownLimit = 5 * time.Second
)
```

Rules: section 4.8, exactly, in its order. Besides:

- The body is decoded with `json.Decoder.DisallowUnknownFields` from an `http.MaxBytesReader`, and anything after the object is a bad request.
- The token never reaches a log line, an error text or the response; the handler hashes it at once and drops the plain value.
- The failure counter is in memory, guarded by a mutex, keyed by the minute of `Now()`.
- Responses are `Content-Type: application/json` (the health check `text/plain`) with `Cache-Control: no-store`; no response echoes request text except the validated decision id.
- `curia serve` logs its listen address and prints it once on stdout (`curia serve: listening on 100.101.102.103:8787`); on a listen error it exits 1 with the cleaned error. It runs `pushLoop` with a kick channel nobody kicks. On `SIGINT` or `SIGTERM` it calls `Shutdown` with `serveShutdownLimit` and exits 0.

- [ ] **Step 1: Write the failing tests** (`httptest.NewServer(serveHandler(…))`, a real store, `tmux.Fake` with the idle screen fixture; tickets created and pushes claimed and marked sent through the store, so a real token exists)

```go
func TestServeRoutes(t *testing.T)                 // GET /healthz -> 200 "ok"; GET /v1/answer -> 405; POST /v1/review, /v1/reviews/R-1/approve, /, /v1/answer/x -> 404; the handler's routes are exactly two
func TestAnswerHappyPath(t *testing.T)             // a valid token for D-1 and key "B" -> 200 with decision, status, delivery; the ticket is answered with answer_key B, answered_via ntfy; the fake got SendKeys with the typed answer; used_at set
func TestAnswerQueuedWhenAgentBusy(t *testing.T)   // the session is working -> 200, delivery holds the queued note, no SendKeys
func TestAnswerAuthTable(t *testing.T)
// no header; "Basic x"; a 42-char token; a token with "+"; an unknown token; the token of another ticket's push; a review push's token;
// a push still sending; a token 25 h old -> 401 each with byte-identical bodies; the ticket still open; nothing typed
func TestAnswerTokenIsSingleUse(t *testing.T)      // the same request twice -> 200 then 401
func TestAnswerBadRequests(t *testing.T)           // Content-Type text/plain; a 2 KiB body; "[]"; {"decision":"D-1"}; an extra field; two JSON objects; decision "D-01"; key "A;B", "" and 9 chars -> 400, the token not spent
func TestAnswerUnknownOption(t *testing.T)         // key "Z" on a ticket with A and B -> 422 "unknown option"; the token is spent; the ticket still open
func TestAnswerNoLongerOpen(t *testing.T)          // the ticket was answered in between -> 409 naming it
func TestAnswerIrreversibleRefused(t *testing.T)   // a valid token on an irreversible ticket (the row made sent by the test) -> 422, the ticket still open
func TestAnswerRateLimit(t *testing.T)             // 10 wrong tokens -> 401 each; the 11th request, even with a right token -> 429; after the clock passes the minute a right token works
func TestAnswerStoreErrorIsInternal(t *testing.T)  // a closed store -> 500 {"error":"internal"}; the cause only in the log
func TestServeNeverLogsOrEchoesTheToken(t *testing.T) // across every test row above: no log line and no response body holds the token
func TestServeResponsesAreClean(t *testing.T)      // a ticket whose label holds ESC: the delivery text of the 200 has no control rune
func TestServeListenAddrTable(t *testing.T)        // flag wins; then cfg; then the first 100.64/10 address of the injected list, port 8787; then fd7a:…; none -> error naming [serve] listen
func TestServeRefusesPublicAddress(t *testing.T)   // flag "0.0.0.0:8787", "192.168.1.5:8787", "[::]:8787" -> error, nothing listens
func TestServeNeedsPublicURL(t *testing.T)         // public_url "" -> exit 1 saying which key to set
func TestServeRefusesInsideAgent(t *testing.T)
func TestServeListensAndShutsDown(t *testing.T)    // --listen 127.0.0.1:0 (the test reads the port from stdout): /healthz answers; cancelling the command's context ends it within 6 s
func TestServeHeaderLimits(t *testing.T)           // a 64 KiB header -> the connection is refused or 431, never a hang
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./cmd/curia/ -run 'Serve|Answer' -v`. Expected: FAIL, unknown command `serve`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./cmd/curia/ -v`. Expected: PASS. No test listens on anything but loopback.
- [ ] **Step 5: Commit.** `git commit -m "feat: curia serve answers a ticket from a one-time push token"`

**Done when:** the nineteen tests pass under `-race`.

### Task 20: Dashboard: the `r` key

**Model:** Opus (UI and a child view).

**Files:**

- Modify: `internal/ui/model.go`, `internal/ui/view.go`, `cmd/curia/ui.go`
- Test: `internal/ui/ui_test.go`, `cmd/curia/ui_test.go`

**Brief includes:** section 4.11 (Dashboard).

**Interfaces:**

- Consumes: `ui.NewReview`, `ReviewOptions`, `ReviewClosedMsg` (Task 8); `newReviewSource` (Task 16); `store.PendingReviewTasks` (Task 6); `model.Session.ReviewPending` (Task 1); the dashboard as Plan 3 leaves it (D13): `WithDecide`, `WithBoard`, the child wrapping of `childMsg`.
- Produces:
  - `ui.Source` gains `Reviews(ctx context.Context) (map[string]string, error)`: the pending review of each task, by task id; only the popup reads it.
  - `func (m Model) WithReview(open func(taskID string) tea.Model) Model`: enables `r`; the child is shown until it sends `ReviewClosedMsg`, wrapped as the decide child is so its late messages are dropped.
  - `func (m Model) WithoutReview(why string) Model`: `r` says why.
  - `cmd/curia/ui.go`: `storeSource.Reviews`; `runDashboard` sets `WithReview` in popup mode when `refuseInsideAgent()` is nil and the log file opened, with `ui.NewReview(newReviewSource(st, client, cfg, logger, decideVia()), clock, ui.ReviewOptions{TaskID: id, Embedded: true})`; otherwise `WithoutReview` with the reason.

Behaviour. `r` on a row whose session has a `TaskID` that is a key of the last good `Reviews` read opens the child for that task; on a team's group row the lead's task counts. On any other row: `no review is waiting for this session`. Without a factory: the reason given, or `reviews cannot be opened inside an agent session (CLAUDECODE is set)`; in the sidebar: `open the popup to review`. A pasted `r` is not a command. A row whose session has `ReviewPending` and state `idle` shows the state icon `◆` in blue (lipgloss colour `4`); every other row is unchanged. The help line gains `r review`. A failed `Reviews` read keeps the last good map.

- [ ] **Step 1: Write the failing tests**

```go
// internal/ui/ui_test.go
func TestRKeyOpensReviewForRowsTask(t *testing.T)     // the factory is called with the row's task id; the child's view is shown
func TestRKeyOnGroupRowUsesLeadTask(t *testing.T)
func TestRKeyWithoutPendingReview(t *testing.T)       // -> the notice, no child
func TestRKeyWithoutFactory(t *testing.T)             // WithoutReview(why) -> why; neither set -> the agent-session notice
func TestRKeyDisabledInSidebar(t *testing.T)
func TestReviewClosedReturnsToDashboard(t *testing.T) // ReviewClosedMsg -> the table again; a late message of the closed child is dropped
func TestPastedRIsNotACommand(t *testing.T)
func TestReviewReadyRowIcon(t *testing.T)             // ReviewPending and idle -> "◆"; ReviewPending and working -> the working icon
func TestReviewsErrorKeepsLastMap(t *testing.T)
func TestHelpLineNamesReview(t *testing.T)
// cmd/curia/ui_test.go
func TestStoreSourceReviews(t *testing.T)             // one pending review -> {T-1: R-1}; a dropped task's review is absent
func TestDashboardOffersReviewInPopupOnly(t *testing.T) // popup outside an agent: r opens; sidebar: the notice; CLAUDECODE=1: the notice
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/ui/ ./cmd/curia/ -run 'RKey|Review|Help' -v`. Expected: FAIL.
- [ ] **Step 3: Implement.** Every existing test's fake `Source` gains `Reviews`.
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/ui/ ./cmd/curia/ -v`. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat: open a review from the dashboard"`

**Done when:** the twelve tests pass.

### Task 21: Badge and ranking in the commands

**Model:** Sonnet (two small changes over finished functions, with tests).

**Files:**

- Modify: `cmd/curia/common.go` (`listSettledMarked`), `cmd/curia/status.go`
- Test: `cmd/curia/common_test.go`, `cmd/curia/status_test.go`, `cmd/curia/next_test.go`, `cmd/curia/ls_test.go`

**Brief includes:** section 4.11 (Badge, Rank).

**Interfaces:**

- Consumes: `store.PendingReviewTasks`, `store.PendingReviews` (Task 6); `fleet.WithReviews` (Task 10); `fleet.Counts.Reviews` (existing); Plan 3's `status.go` with `BadgeExtras` (D5, D15).
- Produces:
  - `listSettledMarked` keeps its signature and, after it has listed, returns `fleet.WithReviews(sessions, pending)` with `pending` from `st.PendingReviewTasks`. A failure of that read is logged to `logPath` and the sessions are returned unmarked. `ls`, `next` and the dashboard's source call it already, so their ranking follows with no other change; `ls --json` keys do not change.
  - `curia status` sets `Counts.Reviews` from `st.PendingReviews` before `fleet.Badge`. On an error of that read it prints nothing, like every other error there. It still calls no tmux, reads no team file and does not call `PendingReviewTasks`. Its cost before and after (one more statement) is measured over 400 sequential runs on a database with 3000 tickets, 3000 messages and 3000 reviews and reported in the PR.

- [ ] **Step 1: Write the failing tests**

```go
// common_test.go
func TestListSettledMarkedMarksReviews(t *testing.T)   // a live session of a task with a pending review -> ReviewPending; others not
func TestListSettledMarkedSurvivesReviewReadError(t *testing.T) // the reviews table dropped by the test -> the sessions, unmarked, and one warning in the log
// status_test.go
func TestStatusCountsPendingReviews(t *testing.T)      // two pending -> the badge ends "R2"; with one open blocking ticket -> "D1 R2"
func TestStatusIgnoresClosedAndDroppedReviews(t *testing.T)
func TestStatusStillSilentOnReviewQueryError(t *testing.T)
// next_test.go
func TestNextJumpsToReviewReadySession(t *testing.T)   // a seen, idle session of a task under review and a working one -> SwitchTo the first
// ls_test.go
func TestLsRanksReviewReadyAfterWaiting(t *testing.T)  // table order: waiting, review-ready, working
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./cmd/curia/ -run 'Review|Status|Next|Ls' -v`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./cmd/curia/ -v`. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat: count reviews in the badge and rank review-ready sessions"`

**Done when:** the seven tests pass and the PR states the two `status` timings.

### Task 22: End-to-end acceptance (Wave 6)

**Model:** Opus (cross-process acceptance).

**Files:**

- Create: `testdata/reviews/` (`client_agent.jsonl`, `config.toml.tmpl`, `summary.txt`), `e2e/reviews_test.go`, `e2e/testdata/reviews/` (`reviews.golden.json`, `tasks.golden.json`, `push_d2.golden.json`, `push_r1.golden.json`, `pane_t1.golden.txt`, `client_agent.golden.jsonl`, `stub_resume.golden.txt`)

**Brief includes:** sections 4.3, 4.4, 4.6, 4.7, 4.8, 4.10.

**Interfaces:**

- Consumes: the built `curia` binary: `task`, `spawn`, `mcp`, `hook`, `review`, `reviews`, `serve`, `restore`, `status`, `ls --json`, `decisions ls`, `messages ls`.
- Produces: the Plan 4 acceptance test.

**Harness.** `e2e/reviews_test.go` reuses the helpers of `replay_test.go`, `decisions_test.go` and `tasks_test.go` (`build`, `newHome`, `normalise`, `startTmux`, `startClient`, `gitInit`, `readGolden`, `compareLines`, the stub `claude` of `testdata/tasks/claude-stub.sh`), reading them and editing none; it stats its own fixtures so a cached pass is not reused after they change. It imports no internal package. It adds:

- A temp git repository `api-server` on `main` with one commit, as Plan 3's test builds it.
- Two loopback HTTP listeners owned by the test: a stand-in ntfy that records every request body, and a free port for `curia serve`. `config.toml.tmpl` is filled with both and written to the temp `HOME`'s `.config/curia/config.toml`: `[ntfy] url`, one device `phone = "curia-e2e"`, `[serve] public_url = "http://127.0.0.1:<port>"`.
- A private tmux server; every run of the binary gets `TMUX=<socket_path>,<pid>,0`, a temp `HOME`, `CURIA_DB` and `CLAUDE_CONFIG_DIR`, `CURIA_NTFY_TOKEN` unset, and `CLAUDECODE` unset except where a step sets it.

**Script.**

1. `curia task add "Fix the auth refresh" --repo <repo>`, `curia spawn T-1`: the stub agent runs in window `T-1`; `curia ls --json` shows its session with `task_id` T-1.
2. An MCP client in the agent's pane raises an assume ticket (`D-1`) and, after the test commits a file on the branch in the worktree, calls `complete_task` with `summary.txt` and `tests_status` `passed`: the result names `R-1`. `curia task ls --json` shows T-1 `review`; `curia status` prints `R1`; within 3 seconds the stand-in ntfy holds one request equal to `push_r1.golden.json`.
3. The client raises a blocking ticket with options A, B, C and recommendation B (`D-2`). Within 3 seconds the stand-in ntfy holds a second request equal to `push_d2.golden.json` after its token is replaced by `TOKEN` and the port by `PORT`: three buttons, B first.
4. `curia serve --listen 127.0.0.1:<port>` runs in the background. `POST /v1/answer` with the captured token and `{"decision":"D-2","key":"B"}` answers 200. `curia decisions ls --status all --json` shows D-2 answered with `answered_via` `ntfy`; within 2 seconds the stub's input record equals the first line of `pane_t1.golden.txt` (the typed answer). The same request again answers 401. `GET /healthz` answers 200.
5. With `CLAUDECODE=1`, `curia review T-1 --approve` exits 1 and the review is still pending.
6. `curia review T-1 --request-changes --note "handle the empty list"` prints `R-1: changes requested; M-1 delivered (send_keys)`; the stub's input record's second line equals the second line of `pane_t1.golden.txt`: `Message M-1 from the user: Review R-1 of T-1: changes requested. handle the empty list`; `curia task ls --json` shows T-1 `running`; `curia status` prints no `R`.
7. The client calls `complete_task` again: `R-2`. `curia review T-1 --approve` prints a line starting `R-2 approved: merged curia/T-1-fix-the-auth-refresh into main`; `git log --merges` on `main` holds one merge whose second parent is the branch; `curia decisions ls --status all --json` shows D-1 `expired`; `curia task ls --status all --json` equals `tasks.golden.json` (T-1 `done`).
8. Restore. The test kills the private tmux server and starts a new one on the same socket name. `curia restore --dry-run` lists the agent's session with the action `restore`; `curia restore` without `--yes` exits 1; `curia restore --yes` prints a line starting `restored`; window `T-1` exists in session `api-server` on the new server; the stub's record of its last start equals `stub_resume.golden.txt` (`--resume`, the session id, no brief); `curia ls --json` shows the session live in its new pane. A second `curia restore --yes` restores nothing.
9. `curia reviews ls --status all --json` equals `reviews.golden.json`; the MCP client's responses equal `client_agent.golden.jsonl`; the log holds no `level=ERROR` line; `curia serve` ends within 6 seconds of `SIGTERM`.

**Normalised before comparing, and nothing else:** every RFC 3339 time becomes `"<time>"`; the temp repository's path (plain and symlink-resolved) becomes `<repo>`; commit ids become `<commit>`; the one-time token becomes `TOKEN` and the serve port `PORT`; `args_hash` values become `<hash>`.

- [ ] **Step 1: Write the failing test**

```go
func TestReviewsEndToEnd(t *testing.T) // steps 1 to 9
```

- [ ] **Step 2: Run and confirm it fails.** Run: `go test ./e2e/ -run Reviews -v`. Expected: FAIL until the fixtures and golden files exist.
- [ ] **Step 3: Write the fixtures and golden files** from the script and section 4, by hand, never from the program's output.
- [ ] **Step 4: Run the whole suite.** Run: `make check`. Expected: every gate passes, the earlier acceptance and latency tests included and unchanged.
- [ ] **Step 5: Whole-plan checks (orchestrator).** `deadcode ./...` without `-test` on `plan-4` reports only deliberate test doubles (`tmux.Fake`). Live check B of section 7 runs.
- [ ] **Step 6: Commit.** `git commit -m "test: end-to-end review, push, phone answer and restore acceptance"`

**Done when:** `TestReviewsEndToEnd` and `make check` pass.

## 13. Decisions made in this plan

Each is: what — why — the alternative and the cost if wrong.

1. **`review` and `done` are derived from a task's newest review; `tasks` is not altered** — Plan 3's rule that the board cannot go stale, and no migration of its `CHECK` — the alternative is storing them; if wrong, a stored `done` is one migration later.
2. **`complete_task` takes `tests_status` as one of `passed`, `failed`, `not_run`, plus an optional one-line `tests_detail`** — the spec's free text cannot be shown consistently or filtered, and an agent that ran nothing needs a word for it — the agent's wording is constrained; the detail line carries the rest.
3. **A second `complete_task` updates the pending review; after a closed one it opens a new review** — the agent often completes twice in a row, and a round of changes should leave a history — two rows per round of changes.
4. **A teammate cannot complete the task, and a session with no task cannot open a review** — only the lead speaks for the job (F14), and a review without a task has no branch to judge — a hand-started session ends with a summary in chat, as before Plan 4.
5. **`complete_task` runs no git; the diff is computed when I look and recorded when I approve** — Ruling 70 (the MCP server never runs git) and work can change after completion — `reviews.diff_stat` is empty until approval.
6. **Approve defaults to a local `git merge --no-ff --no-edit` in my checkout, and only when the base is checked out there** — local, reversible and offline; the spec leaves the choice to config — the alternative default is a pull request; if wrong, one config line (question 1). A base that is not checked out is refused instead of updated behind my back.
7. **The merge uses git's own message and runs the repository's hooks** — a custom message would fight commit-msg checkers that exempt `Merge branch …`, and skipping hooks is not Curia's call — a hook that rejects merge commits makes me approve with `--pr` or merge by hand.
8. **Approval expires the task's open assume tickets, in both modes, and blocking tickets are left open** — F8's "or when its task merges"; approving is the merge decision, and I have just seen them in red — in pull-request mode they expire before the PR is merged; an expired ticket is not precedent, which is what the spec wants.
9. **In pull-request mode the review ends `approved`; nothing later flips it to `merged`** — detecting a merge needs the network or fails on squash merges — `reviews ls` shows `approved (pr)` for good; the task shows `done` either way.
10. **The agent's summary goes into the pull-request body inside a fence it cannot close, and `@` is removed from the title** — agent text on GitHub can mention people and embed links — the summary is not rendered as markdown.
11. **A change request is a Plan 3 message addressed to the task's live lead, else queued for the review's session** — one typing path with every guard, and the note survives `resume` and `restore`, which keep the session id — this departs from Plan 3's decision 35 (an instruction for a task with no live agent is refused) for review notes only; a task spawned afresh gets a new session and never receives a note queued for the old one (it stays in `curia review` and `messages ls`).
12. **Dropping a review drops the task and does not stop its agent** — the spec says "`x` drops the task"; killing panes belongs to the dashboard's `x` — a dropped task's agent keeps running until I kill it; the outcome line says so.
13. **The diff tool is one of four fixed commands, not a command string** — the spec names three tools; a free string needs splitting or a shell — another tool needs a code change.
14. **Pushes go through an outbox table and are sent by `curia mcp` processes and `curia serve`; there is still no daemon** — quiet hours need something alive at their end, and every live agent already has a `curia mcp` — a push held while no agent session and no `curia serve` runs waits for the next one to start; the 60 second tick is its resolution.
15. **Every new blocking ticket is pushed; the 30 second limit of the bell does not apply; assume tickets are never pushed and there is no option for it** — each push carries its own buttons, and the spec's "by default" names no user of the opposite — a runaway agent can send many pushes (each distinct ticket once per device); an option is one config key later.
16. **A phone button carries a one-time token bound to one ticket and one device's push, stored as a hash; the spec's "per-device token" becomes per-device topics** — a long-lived device token would have to sit in clear on the machine and in every notification; a device is revoked by removing its topic — anyone who can read the topic within 24 hours and reach the endpoint can answer that one ticket once (question 2).
17. **At most three buttons, the recommended option first; an irreversible ticket gets none, and the endpoint refuses it** — ntfy shows three actions; a lock-screen tap must not decide something that cannot be undone — a fourth option and every irreversible ticket need `curia decide` over SSH.
18. **A review is pushed without buttons; nothing can be approved from the phone** — the spec lists option keys only, and a merge from a notification is too easy — reviews wait for a terminal (question 3).
19. **`curia serve` binds only to loopback or a Tailscale address and has no TLS of its own** — spec section 7; the tunnel or `tailscale serve` carries it — a setup that needs another interface needs a code change.
20. **The blue window marker for review-ready (F4) is built by changing only the value of `@curia_marker_format`** — the installer fix keeps the marker in that option, so no format, key or other option is touched (Rulings 94 to 96) and a re-source upgrades a running server — blue means "a review arrived since I last looked at this pane": it is set when the review opens and cleared by focus, and a blue dot can show on the window I am watching until the next focus change (Ruling 38's refinement for red is not repeated for blue); marker-changing hook events cost one more indexed read.
21. **A session's tmux server is recorded as its socket path, from the tmux call the hook already makes** (Plan 2 M7, Plan 1 Open Question 13) — no added process on the hook path, and restore, reconcile and typing all need to tell servers apart — rows from before the upgrade have no socket until their next SessionStart and behave as before.
22. **`curia restore` needs a running tmux server, shows its plan and asks before acting** — it must know which server it restores into, and relaunching agents is not something to do on a guess — one more step after a reboot (start tmux first); `--yes` for scripts.
23. **What is offered for restore is decided by the transcript's modification time, within 24 hours by default** — `updated_at` is reset when a row is marked stale (Plan 1 live-check defect 12), and the transcript is what `--resume` needs anyway — a session idle for more than a day before the crash needs `--since`.
24. **Restore opens one new window per session and never types into or reuses a pane** — the only safe way to start a program in tmux here is `NewWindow` with a validated argv — pane layouts are not restored; tmux-resurrect does that, and its empty shells are mine to close.
25. **An unlinked session is relaunched as plain `claude --resume <id>`; a linked one through `curia launch`** — Curia knows nothing more about a hand-started session; no permission flag is ever added — a session started with special flags comes back without them.
26. **A restored team lead is told to respawn its team by a message, and its teammates' rows are ended** — the spec's rule; a message reaches it by typing, the Stop hook or `check_messages` — if typing is refused after a resume (probe R1) the notice arrives at the lead's next Stop.
27. **Quiet hours hold back only ntfy pushes** — the spec says "no pushes" — a bell or a macOS notification can still sound at night; both are off by default.
28. **The review shows a cost only when a linked session has one stored** — nothing writes `sessions.cost_usd` until Plan 5 — the line reads `cost not recorded` until then; the team total needs no further change.
29. **The board hides `done` until `D` is pressed, and offers no review action** — done tasks accumulate; the review opens from the dashboard's `r` or `curia review` — one more key to see finished work.
30. **`curia reviews ls` is a separate, read-only command** — `curia review` is user-only and has positional ids; listing is what scripts and the acceptance test need, as `decisions` is to `decide` — two commands for one feature.
31. **The ntfy access token comes only from `CURIA_NTFY_TOKEN`; topics live in the config file** — the owner keeps secrets in the Keychain, exported as environment variables; a topic is a capability name, not a credential — a config file in a dotfiles repository exposes the topics (question 5).

## 14. Live probes before Wave 1

Nothing below has been verified by anyone; the tasks named depend on the answers. The orchestrator runs each probe before section 6's setup PR, records the result as a Ruling and in `AGENTS.md`, and amends the named tasks before dispatch.

**Method for every probe and live check** (Ruling 33 and Plan 3's section 15): a private tmux server (`tmux -L curia-probe-<pid> -f /dev/null`, `TMUX_TMPDIR` in a short temp directory); Curia's state through a wrapper script that sets `HOME` and `CURIA_DB` to a temp directory before it runs the built binary; real `claude` only as `claude --settings <temp file> --setting-sources project [--mcp-config <temp file> --strict-mcp-config]`, whose hook and MCP commands are that wrapper, started in a throwaway git repository under a temp directory; `--model haiku`. The real `~/.claude/settings.json`, `~/.tmux.conf`, `~/.config/curia` and the default tmux server are never touched. ntfy is a local `ntfy serve` with a temp config on `127.0.0.1` when the binary is available, else `https://ntfy.sh` with a random 32-character topic and placeholder text only; no real ticket text and no real topic. Known side effects, as before: the trust dialog writes the probe directory into the real `~/.claude.json`, and transcripts land in the real `~/.claude/projects`. Throwaway files live in `.scratch/`; reports in `.superpowers/sdd/2026-11-13-curia-plan-4/`.

Verified at the desk while writing this plan, and so not probes: the migration's DDL on SQLite 3.51.0 after Plan 3's 0004; tmux 3.8's `#{socket_path}` (an absolute path under `/private/tmp/tmux-<uid>/`); git 2.56.0's `diff --stat <base>...<branch>` from the main checkout, `merge --no-ff --no-edit` with an unrelated and with an overlapping local change, a conflict and `merge --abort`, a failing commit-msg hook leaving `MERGE_HEAD`, `symbolic-ref --short -q HEAD` on a detached head (exit 1), `rev-list --count`, `push --set-upstream <remote> <b>:<b>` to a bare repository; and that Claude Code 2.1.325's `--help` lists `-r/--resume [value]` and `--fork-session` ("create a new session ID instead of reusing the original"), which implies a resume keeps its id. Also at the desk, with the pinned `golangci-lint` and this repository's `.golangci.yml` on a 40-line stand-in: an `http.NewRequestWithContext` whose URL was read from the config file under the home directory, a `client.Do` of it, and a `net.Listen` on an address from that file pass with no finding; the same request with a URL derived from a command-line argument is reported twice as `G704: SSRF via taint analysis`. So the ntfy URL comes only from the config file, never from a flag or an argument (Tasks 7 and 15), and no suppression is needed.

| # | Question | Experiment | If yes / as assumed | Otherwise |
| --- | --- | --- | --- | --- |
| R1 | After its process was killed (once at an idle prompt, once mid-turn with `kill -9`) and the tmux server was killed, does `claude --resume <id>` run as a window's direct command (no shell) in the same directory come back with the same session id, fire SessionStart with source `resume`, and show the idle prompt and footer that `deliver.readPrompt` recognises? Does any dialog come first (the trust dialog, which in Claude Code 2.1.325 defaults to "No, exit"; a "resume from summary" choice on a long transcript), and which footer does the idle prompt carry (Ruling 101's `· /tasks to see subagents`)? | Start a session through the wrapper, give it two prompts, kill it both ways, start a new private server, open a window whose command is `claude --settings … --resume <id>`; record every hook payload and capture the screen into `internal/deliver/testdata/prompt/p4-idle-after-resume.txt` (and each dialog seen). | Same id, source `resume`, idle prompt recognised: section 4.10 stands; the captured screen becomes a fixture of Task 14's and Plan 3's typing tests. | A new id: nothing changes in the code (step 6 handles it); section 4.6's note about queued change requests is amended to say they are lost on restore. A dialog first: `restore` prints, per window, that the agent waits at a dialog, and the 30 second wait counts it as not reported (Task 18's wording amended). The idle prompt not recognised: the team notice arrives by the Stop hook only; decision 26's cost becomes the rule and Task 18's test for the typed notice is dropped. Resume fails outright without a shell: Task 18's unlinked command becomes `[<bin>, "launch-raw", …]` only if Plan 3's `launch` can serve it; otherwise unlinked sessions are listed with the command to run by hand and are not restored. |
| R2 | Which model does a resumed session use: the one it ended on, or the default? | In R1's setup start the session with `--model haiku`, resume it without `--model`, read `model` from the SessionStart payload and the next transcript line. | The session's own: nothing changes. | The default: `restoreSpec` adds `--model <sessions.model>` for an unlinked session when `task.ValidModel` accepts the stored value (Task 18 amended). |
| R3 | Agent Teams: after R1's kill, what does a resumed lead (with the switches Plan 3's P5 established) see of its former teammates, do their panes and team config entries linger, and does it respawn a team when told by a message? | Build a team of two as in Plan 3's P5, kill everything, resume the lead through `curia launch … --resume … --team`, deliver the notice text by hand as a prompt; record `tmux list-panes -a`, the team directory and the hooks. | The lead respawns teammates as new sessions: section 4.10 step 7 stands. | Stale teammates in the config make Plan 3's `team.Assign` match dead rows: step 7 also ends them (already the rule) and the orchestrator records whether the config must be left alone or the lead cleans it. The lead cannot form a team after a resume: the notice text becomes `… Your teammates did not survive it: continue alone, or ask the user to respawn the task.` (the constant in Task 18). |
| N1 | Does ntfy accept section 4.7's JSON publish at the server's base URL, with an `http` action carrying headers and a body, `clear`, and three actions; what does it answer for a fourth action, a 5000-character message, a bad topic, and when rate-limited? | Send the request of section 4.7 with placeholder text to the probe server; then the variants. Record one accepted request (token replaced by `TOKEN`) as `internal/notify/testdata/ntfy/publish-accepted.json` and its reply as `publish-reply.json`; note every status code. | As assumed: only the fixtures are new. | Another body shape or field name: section 4.7's request and Task 7 are amended to the recorded one. Fewer or more actions allowed: `notify.MaxActions` follows. Headers or body not allowed in an action: the token moves into the action URL's path (`/v1/answer/<token>`), section 4.8's step 3 and Tasks 7, 19 and 22 are amended, and the log rule gains "never the path". |
| N2 | Does a `curl` to the stand-in for the endpoint, shaped exactly like the phone's action request ntfy documents (method, headers, body), match what Task 19 expects, and what does the ntfy app send as `Content-Type` and `User-Agent`? | Publish N1's message to a topic a desktop ntfy client (the `ntfy subscribe` CLI or the web app on loopback) is subscribed to, trigger the action there against a local listener, record the request. | `Content-Type: application/json` as set by the action's headers: section 4.8 stands. | The client overrides or drops the header: step 2 of section 4.8 accepts the recorded type too (Task 19 amended). |

Not probed, and why: `gh pr create` against GitHub would open a real pull request, which is a visible action on someone's account; Task 16 rests on `gh`'s documented behaviour (the URL on stdout) and question 7 of section 15 asks the owner to run it once on a throwaway repository. The phone itself is the owner's (question 6).

## 15. Questions only the owner can answer

Each has the default this plan builds and blocks nothing.

1. **Approve: merge locally, or open a pull request?** Default: `merge` (`git merge --no-ff` into the base in your checkout, never pushed); `review.approve = "pr"` or `--pr` for the other.
2. **The spec asks for a per-device token; this plan gives each device its own topic and each notification a one-time token** (decision 16). Default: as planned. The alternative, a long-lived token per device, would be stored in clear on this machine and sent in every notification.
3. **Should a review be approvable from the phone?** Default: no; the push only tells you it is waiting.
4. **The blue window marker clears when you focus the pane, while the review still waits** (the spec's words; decision 20). Default: as the spec says; the badge's `R` and the dashboard keep showing it. Say so if blue should stay until the review is closed.
5. **ntfy topics in `~/.config/curia/config.toml`.** Your dotfiles are managed by chezmoi; a topic is what lets someone read your ticket text. Default: the config file, with the access token only in `CURIA_NTFY_TOKEN` (a Keychain item `env:CURIA_NTFY_TOKEN`). Say so if topics should come from the environment too.
6. **A check only your phone can do:** whether the ntfy app performs an action against `http://<tailscale ip>:8787`, or needs `https` (then `tailscale serve` in front of a loopback listener, with `public_url` set to the `https://<machine>.<tailnet>.ts.net` address), and whether `curia decide` is usable in your phone's SSH app at its width (Plan 2 Open Question 21 asked the same about bracketed paste). Default: both forms are supported; nothing depends on the answer.
7. **One real `gh pr create`** on a throwaway repository of yours, to confirm Task 16's pull-request path end to end. Default: not run; the path is tested against a local bare remote and a stand-in `gh`.
8. **Quiet hours and the bell.** Default: quiet hours hold back only ntfy pushes (decision 27).
9. **Restore's window.** Default: sessions active in the last 24 hours, shown and confirmed before anything starts (decisions 22, 23).
10. **Dropping from a review leaves the agent running** (decision 12). Default: yes; kill it from the dashboard.

## 16. Out of scope

Waiting for Plan 5:

- The Linear adapter, Codex (`--agent codex`, and restoring a Codex session), Obsidian export, Agent Commons (F13).
- `curia ingest`, cost and cache columns, per-branch cost from the cost-lens analyzer in the review (F15); the review shows a stored cost as soon as one exists.
- Usage forecasting and budget-aware scheduling (F16); the cache stakes line in a push (F17).

Deferred items of Plans 1 to 3 that are picked up here: the tmux server identity (Plan 2 final-review M7, Plan 1 Open Question 13; Tasks 2, 6, 10, 13, 14, 18); F8's expiry when a task merges and its review link (Tasks 6, 8, 16); the badge's `R`, zero since Plan 1 (Task 21); the `answered_via` value `ntfy` reserved by migration 0002 (Task 19); what Plan 3's section 17 assigned to Plan 4 (`complete_task` in the brief, the `review` and `done` statuses and their board columns, per-team totals in the review).

Still deferred, and not picked up here: the closing "Continue." of the typed answer; a second chance for an unconfirmed typed line at the next Stop; the stale `SubagentStop` count after a resume (Plan 1 Open Question 18, which a restore can now trigger: the count is reset by the resumed session's SessionStart, so it does not apply to a restored session); `curia forget` for sessions outside tmux; a stale row's age reset (Plan 1 defect 12; restore reads the transcript's time instead); uninstalling entries of a moved binary; the minors listed per task in `PROGRESS.md`.

Not planned: approving a review from the phone (decision 18); pushes for assume tickets or for messages (decision 15); a command that lists the push outbox; flipping `approved` to `merged` after a pull request merges (decision 9); a per-repo approve mode; restoring pane layouts or sessions outside tmux (decision 24); starting `curia serve` from `curia install` or at login; TLS in `curia serve` (decision 19); a key binding for `curia review` (it opens from the dashboard's `r`), or anything in the tmux snippet beyond the marker's value.

## Appendix A: the interface and file check

The check the plan was held to before it was finished. Two tables: where every consumed interface comes from, and which files each wave touches.

**A.1 Consumed interfaces.** "main" means the code at 33db0e4 (file given); "P3" means Plan 3's plan (its task given), to be re-verified per the dependency table (row given).

| Task (wave) | Consumes | Produced by |
| --- | --- | --- |
| 1 (1) | nothing | |
| 2 (1) | `paneFormat`, `parsePanes` | main, `internal/tmux/exec.go` |
| 3 (1) | `toolRules`, `AllowTools`, `ToolsAllowed`, `Unhooks`; five rules; skill text | main, `internal/install/settings.go`; P3 Tasks 10, 21 (D12) |
| 4 (1) | the board, `task.Card`, `model.TaskReview`, `model.TaskDone` | P3 Tasks 20, 6, 1 (D13, D8, D2) |
| 5 (2) | `model.Review`, `TestsStatus`, `MaxReviewNote` | Task 1 (wave 1) |
| | `task.Problem`, `task.ValidID`, `task.Brief` | P3 Task 6 (D8) |
| | `sanitize.Text`, `sanitize.Line` | main, `internal/sanitize/sanitize.go` |
| 6 (2) | `model.Review`, enums, `Session.Socket` | Task 1 (wave 1) |
| | `begin`, `formatTime`, `parseTime`, `selectDecisions`, `scanDecision`, `upsert`, `selectSessions`, `scanSession` | main, `internal/store/decisions.go`, `store.go` |
| | `tasks` table, `ErrNoTask` | P3 Task 8 (D1, D4) |
| 7 (2) | `config.ParseQuiet`, `model.Review` | Task 1 (wave 1) |
| | `repoPart`, `model.Decision`, `model.Task` | main, `internal/notify/notify.go`; P3 Task 1 (D2) |
| 8 (2) | `model.Review`, `ApproveHow`, `MaxReviewNote` | Task 1 (wave 1) |
| | `dwell`, `quietPeriod`, `answeredQuiet`, `fit`, `truncate`, `Age` | main, `internal/ui/decide.go`, `view.go` |
| 9 (3) | `model.Push`, enums | Task 1 (wave 1) |
| | `pushes` table | Task 6 (wave 2) |
| | `meta`, `ClaimNotifySlot`'s statement | main, `internal/store/housekeeping.go` |
| 10 (3) | `Session.Socket`, `ReviewPending` | Task 1 (wave 1) |
| | `Pane.Socket` | Task 2 (wave 1) |
| | the stored socket, `store.PendingReviewTasks` | Task 6 (wave 2) |
| | `Reconcile`, `goneFrom`, `markStale`, `group`, `Rank`, `Next`; `attention.Marker`, `WindowMarker`, `Apply`; `markerFormat` | main, `internal/fleet/fleet.go`, `internal/attention/attention.go`, `internal/install/tmuxconf.go` (D18) |
| 11 (3) | `store.LatestReviews` | Task 6 (wave 2) |
| | `Derive`, `Cards`, `Card`, `taskCards` | P3 Tasks 6, 16 (D8, D11) |
| 12 (3) | `task.Completion`, `NormalizeCompletion`, `ValidateCompletion` | Task 5 (wave 2) |
| | `store.CompleteTask` | Task 6 (wave 2) |
| | `mcp.Deps`, `Tools`, `wrap`, `decodeArgs`, `invalidArguments`, `ToolError`, `Problem` | main, `internal/mcp/tools.go`, `server.go` |
| | `Caller.TaskID`, `Caller.TeamRole`, five tools | P3 Task 26 (D12) |
| 13 (3) | `Pane.Socket` | Task 2 (wave 1) |
| | the stored socket | Task 6 (wave 2) |
| | `hook.Env`, `startInfo`, `apply`, `hookEnv` | main, `internal/hook/run.go`, `cmd/curia/hook.go`; P3 Task 14 (D14) |
| 14 (4) | `Pane.Socket` | Task 2 (wave 1) |
| | `Session.Socket` in the claims | Task 6 (wave 2) |
| | `deliver.Answer`, `paneRefusal`, `requeue`; `store.TypingClaim`, `RequeueDelivery` | main, `internal/deliver/deliver.go`, `internal/store/delivery.go` |
| | `deliver.Message`, `store.MessageClaim`, `RequeueMessage` | P3 Tasks 17, 12 (D6) |
| 15 (4) | `notify.NotBefore`, `DecisionPush`, `ReviewPush`, `NewToken`, `Publisher`, `ErrRejected` | Task 7 (wave 2) |
| | `attention.Apply` with the blue marker | Task 10 (wave 3) |
| | `store.EnqueuePushes`, `PushesDue`, `ClaimPush`, `FinishPush`, `PushMaxAttempts` | Task 9 (wave 3) |
| | `mcp.Deps.Reviewed` | Task 12 (wave 3) |
| | `store.GetReview` | Task 6 (wave 2) |
| | `config.Ntfy.Enabled`, `DeviceNames`, `config.Serve` | Task 1 (wave 1) |
| | `mcpServe`, `mcpNotify`, `store.GetDecision`, `clock` | main, `cmd/curia/mcp.go`, `common.go`, `internal/store/decisions.go` |
| | `store.GetTask` | P3 Task 8 (D4) |
| 16 (4) | `ui.NewReview`, `ReviewSource`, `ReviewRow`, `ReviewDetail`, `ReviewOptions` | Task 8 (wave 2) |
| | `store.GetReview`, `ListReviews`, `CloseReview`, `ReviewClose`, `SetReviewNoteMessage`, `TaskDecisions`, `TaskRun`, `ErrNoReview`, `ErrReviewClosed` | Task 6 (wave 2) |
| | `task.ValidReviewID`, `NormalizeNote`, `NoteMessage`, `PRTitle`, `PRBody` | Task 5 (wave 2) |
| | `config.Review` | Task 1 (wave 1) |
| | `runGit`, `gitEnv`, `refuseInsideAgent`, `decideVia`, `runProgram`, `jsonTime`, `applyMarkers` | main, `cmd/curia/git.go`, `decisions.go`, `decide.go`, `ui.go`, `ls.go`, `common.go` |
| | `store.GetTask`, `TaskSessions`, `CreateMessage`; `deliver.Message`; `task.ValidID`, `ValidBranch`; `worktreeDirty`, `refExists`, `sendOutcomeLine` | P3 Tasks 8, 12, 17, 6, 19, 25 (D4, D5, D6, D8, D11) |
| 17 (4) | `store.ListReviews`, `ReviewFilter` | Task 6 (wave 2) |
| | `ui.Cell`, `ui.Age`, `jsonTime`, `escapeUnprintable` | main, `internal/ui/view.go`, `cmd/curia/ls.go` |
| | `store.ListTasks` | P3 Task 8 (D4) |
| 18 (4) | `Pane.Socket` | Task 2 (wave 1) |
| | `Session.Socket` | Task 6 (wave 2) |
| | `store.ClaimRestore`, `ReleaseRestore` | Task 9 (wave 3) |
| | `config.Restore` | Task 1 (wave 1) |
| | `store.ListSessions`, `GetSession`, `UpdateSession`; `executablePath`; `refuseInsideAgent` | main, `internal/store/store.go`, `cmd/curia/install.go`, `decisions.go` |
| | `tmux.WindowSpec`, `NewWindow`; `task.SessionName`, `ValidID`, `ValidModel`; `store.GetTask`, `ListTasks`, `UpdateTask`, `CreateMessage`; `deliver.Message`; `sendOutcomeLine`; `curia launch` | P3 Tasks 3, 6, 8, 12, 17, 25, 19 (D9, D8, D4, D5, D6, D11, D10) |
| 19 (5) | `store.UsePushToken`, `ErrBadToken` | Task 9 (wave 3) |
| | `notify.HashToken` | Task 7 (wave 2) |
| | `pushLoop` | Task 15 (wave 4) |
| | `model.AnsweredNtfy`, `config.Serve`, `config.TailscaleAddr` | Task 1 (wave 1) |
| | `deliver.Answer`; `store.GetDecision`, `ErrNotOpen`, `ErrNotFound`; `decision.ValidID`, `ErrBadAnswer` | main, `internal/deliver/deliver.go`, `internal/store/decisions.go`, `internal/decision/request.go`, `resolve.go` |
| 20 (5) | `ui.NewReview`, `ReviewOptions`, `ReviewClosedMsg` | Task 8 (wave 2) |
| | `newReviewSource` | Task 16 (wave 4) |
| | `store.PendingReviewTasks` | Task 6 (wave 2) |
| | `Session.ReviewPending` | Task 1 (wave 1) |
| | `ui.Model`, `Source`, `WithDecide`, `childMsg`, `stateIcon`; `runDashboard`, `storeSource` | main, `internal/ui/model.go`, `view.go`, `cmd/curia/ui.go`; P3 Task 30 (D13) |
| 21 (5) | `store.PendingReviewTasks`, `PendingReviews` | Task 6 (wave 2) |
| | `fleet.WithReviews` | Task 10 (wave 3) |
| | `listSettledMarked`, `statusBadge`, `fleet.Counts.Reviews` | main, `cmd/curia/common.go`, `status.go`, `internal/fleet/fleet.go`; P3 Tasks 22, 29 (D11, D15) |
| 22 (6) | the built binary's commands | Tasks 12, 15 to 21 (waves 3 to 5); P3 |
| | `testdata/tasks/claude-stub.sh`, the e2e helpers | P3 Task 31 (D16); main, `e2e/replay_test.go`, `decisions_test.go` |

No task consumes something produced in its own wave or a later one. This was checked a second time for types and constants alone, which is where Plan 3 slipped (its Task 4 needed constants its Task 1 created in the same wave): every type and constant a task names is either in `main`, in Plan 3, or created by a task of an earlier wave. The ones that would have crossed a wave were moved: `model.MaxReviewNote` lives in Task 1 so that Tasks 5 and 8 (both wave 2) share it; `task.Completion` (Task 5, wave 2) is first used in wave 3; `store.PushMaxAttempts` and `notify.MaxActions` (waves 3 and 2) are first used in wave 4; `ui.ReviewDetail` (Task 8, wave 2) is first implemented against in wave 4. Within a wave, tasks share only names that existed before the wave began.

**A.2 Files per wave.** A file appears at most once in a row.

| Wave | Files created or modified (task) |
| --- | --- |
| 1 | `internal/model/review.go`, `model.go`, `decision.go`, `internal/config/config.go`, `config_test.go` (1); `internal/tmux/tmux.go`, `exec.go`, `tmux_test.go` (2); `internal/install/plan4_test.go`, `settings.go`, `plan2_test.go`, `skill/SKILL.md`, `cmd/curia/install.go`, `install_test.go` (3); `internal/ui/board.go`, `board_view.go`, `board_test.go` (4) |
| 2 | `internal/task/review.go`, `review_test.go`, `brief.go`, `brief_test.go`, `e2e/testdata/tasks/stub_argv.golden.txt` (5); `internal/store/migrations/0005_reviews.sql`, `reviews.go`, `reviews_test.go`, `store.go`, `store_test.go` (6); `internal/notify/ntfy.go`, `ntfy_test.go` (7); `internal/ui/review.go`, `review_view.go`, `review_test.go` (8) |
| 3 | `internal/store/pushes.go`, `pushes_test.go`, `housekeeping.go`, `housekeeping_test.go` (9); `internal/fleet/fleet.go`, `fleet_test.go`, `internal/attention/attention.go`, `attention_test.go`, `internal/install/tmuxconf.go`, `tmux_real_test.go` (10); `internal/task/status.go`, `status_test.go`, `cmd/curia/task.go`, `task_test.go` (11); `internal/mcp/complete.go`, `complete_test.go`, `tools.go`, `tools_test.go`, `cmd/curia/mcp_test.go`, `e2e/testdata/decisions/tools_list.golden.json` (12); `internal/hook/run.go`, `hook_test.go`, `cmd/curia/hook.go`, `hook_test.go` (13) |
| 4 | `internal/deliver/deliver.go`, `deliver_test.go`, `message.go`, `message_test.go` (14); `cmd/curia/push.go`, `push_test.go`, `mcp.go` (15); `cmd/curia/review.go`, `review_test.go`, `review_git.go`, `review_git_test.go` (16); `cmd/curia/reviews.go`, `reviews_test.go`, `cmd/curia/testdata/reviews.golden.json` (17); `cmd/curia/restore.go`, `restore_test.go` (18) |
| 5 | `cmd/curia/serve.go`, `serve_test.go` (19); `internal/ui/model.go`, `view.go`, `ui_test.go`, `cmd/curia/ui.go`, `ui_test.go` (20); `cmd/curia/common.go`, `common_test.go`, `status.go`, `status_test.go`, `next_test.go`, `ls_test.go` (21) |
| 6 | `testdata/reviews/`, `e2e/reviews_test.go`, `e2e/testdata/reviews/` (22) |

Checked by hand for the three places where names are close: `internal/ui/review_test.go` (Task 8) and `cmd/curia/review_test.go` (Task 16) are in different directories and waves; `cmd/curia/mcp_test.go` (Task 12, wave 3) and `cmd/curia/mcp.go` (Task 15, wave 4) are in different waves, and Task 15's tests of the MCP wiring go in `push_test.go`; `internal/hook/hook_test.go` and `cmd/curia/hook_test.go` are both Task 13's.
