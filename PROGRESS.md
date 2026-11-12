# PROGRESS

This file mirrors the plan for humans; it does not replace it. Task order, interfaces and tests come from `docs/superpowers/plans/2026-11-11-curia-plan-1-foundation.md`.

## Status

- **Plan 1 (Foundation): complete.** All eleven tasks, the final whole-branch review and its fix wave are merged into `plan-1` (PRs #2 to #13). The fix-wave re-review found every finding addressed and nothing new at Critical or Important level.
- `deadcode ./...` without `-test` reports only `tmux.Fake`, the deliberate test double.
- The owner (2026-11-11) authorised the orchestrator to merge its own PRs, including `plan-1` into `main`, and to write and execute Plan 2 and later plans. An isolated live check (private tmux server, temp HOME) is authorised; the real `curia install` stays with the owner.
- Not yet proven against a live Claude Code and tmux setup on the owner's machine.

## Plan 1 waves

Every task's done criteria also include: `make check` passes in the task's worktree; the PR into `plan-1` uses the PR template; CI (`check`) is green; the task review is clean or every finding is parked with a ruling.

Model column: the expected starting point from the owner's prompt, **expected, confirmed at dispatch**. The orchestrator confirms or changes it, with a one-line reason, before dispatching.

### Wave 1

| Task | Title | Model (expected, confirmed at dispatch) | Done criteria | Status |
| --- | --- | --- | --- | --- |
| 1 | Scaffold, shared types and config | Sonnet (confirmed: 3 files, exact signatures and tests in the plan, no concurrency or UI) | `internal/model` types and constants exactly as the Interfaces block; `config.Default`, `config.Load`; `TestDefaults`, `TestLoadMissingFileReturnsDefaults`, `TestLoadOverrides`, `TestCuriaDBEnvOverride` pass; `go build ./cmd/curia` works. `go.mod` and `cmd/curia/main.go` already exist (Ruling 3): Task 1 does not edit them. | merged (PR #2) |

### Wave 2

| Task | Title | Model (expected, confirmed at dispatch) | Done criteria | Status |
| --- | --- | --- | --- | --- |
| 2 | Store | Opus (confirmed: concurrent writers, WAL and busy_timeout behaviour) | `store.Open` (WAL, `busy_timeout=2000`, embedded migrations recorded in `schema_migrations`), Upsert as one `INSERT ... ON CONFLICT` statement, Get, List with `ListFilter`, AppendEvent, MarkSeen, EndOthersInPane; `0001_init.sql` matches spec section 5 with the two stated differences; `TestOpenCreatesSchemaAndWAL`, `TestOpenIsIdempotent`, `TestUpsertRoundTrip`, `TestListFiltersEndedAndOldStale`, `TestMarkSeen`, `TestEndOthersInPane`, `TestConcurrentUpserts` pass under `-race`. | merged (PR #7) |
| 3 | State machine | Sonnet (confirmed: one pure function, full rule table in the plan) | `state.Apply` pure (no I/O, no clock), one switch on `ev.Kind`, every rule in the plan's table; `TestApplyTransitions`, `TestEndedIgnoresEventsExceptSessionStart`, `TestSubagentCountNeverNegative`, `TestStopWithOpenDecision`, `TestLastUpdateTruncatedTo80Runes` pass. | merged (PR #3) |
| 4 | Transcript reader | Sonnet (confirmed: one function, exact rules and tests in the plan) | `transcript.Read` with the plan's rules (usage lines only, synthetic and malformed skipped, partial trailing line not consumed, missing file, shrunk file re-read from 0); fixtures in `internal/transcript/testdata/`; `TestReadsLatestUsage`, `TestIncrementalOffset`, `TestPartialTrailingLine`, `TestMissingFile`, `TestShrunkFileRereadsFromZero`, `TestSkipsSyntheticAndMalformed`, `TestLastAssistantTextFirstLine` pass. | merged (PR #4) |
| 6 | tmux adapter | Sonnet (confirmed: thin exec wrapper with exact signatures; the process boundary is one CLI call per method) | `tmux.Pane`, `tmux.Client` (6 methods), `tmux.NewExec(socket)`, `tmux.Fake` with `Panes` and `Calls`; `TestParseListPanes` passes; `TestExecAgainstRealTmux` runs (not skipped) locally and in CI against `tmux -L curia-test`, and kills its server in `t.Cleanup`. | merged (PR #5) |
| 10 | Install and uninstall | Sonnet (confirmed: file merge logic with exact rules and tests, no concurrency) | `install.Hooks`, `Unhooks`, `WriteTmux`, `RemoveTmux` per the plan's rules (backup before write, user entries preserved, invalid JSON writes nothing, fence added once); `TestHooksMergePreservesUserHooks`, `TestHooksIdempotent`, `TestHooksBackupWritten`, `TestInvalidJSONUntouched`, `TestMissingSettingsCreated`, `TestUnhooksRemovesOnlyOurs`, `TestTmuxFenceAddedOnceAndRemoved` pass; `curia install` / `uninstall` in `cmd/curia/install.go`. | merged (PR #6) |

### Wave 3

| Task | Title | Model (expected, confirmed at dispatch) | Done criteria | Status |
| --- | --- | --- | --- | --- |
| 5 | Hook command | Opus (confirmed: integrates four packages, cross-process locking, latency budget) | `hook.Parse`, `hook.Env`, `hook.Run` in the plan's order; one payload fixture per event kind; `curia hook <Event>` exits 0 on every path and logs errors; `TestParseFixtures`, `TestRunSessionStartRecordsSession`, `TestSessionStartEndsPriorSessionInPane`, `TestHookWithoutTmuxPane`, `TestStopReadsTranscript`, `TestHookLatency` (p95 < 20 ms), `TestMalformedPayloadExitsZero`, `TestHookExitsZeroWhenStoreLocked` pass under `-race`. | merged (PR #8) |

### Wave 4

| Task | Title | Model (expected, confirmed at dispatch) | Done criteria | Status |
| --- | --- | --- | --- | --- |
| 7 | Fleet ranking, badge, ls, status, next | Opus (changed from Sonnet: seven files across `fleet` and three commands, plus a cross-process reconcile under Ruling 14) | `fleet.Reconcile`, `Rank`, `Counts`/`Count`, `Badge`, `Next`; `curia ls [--json]`, `curia status`, `curia next` in their own files; `TestReconcileMarksMissingPanesStale`, `TestReconcileIgnoresSessionsWithoutPane`, `TestRankOrder`, `TestRankOldestFirstWithinGroup`, `TestBadge`, `TestNextSkipsSessionsWithoutPane`, `TestNextNoneWhenAllWorking`, `TestLsJSONGolden` pass. | merged (PR #10) |
| 8 | Attention markers and mark-seen | Sonnet (confirmed: one small package and one command, exact signatures and tests) | `attention.Marker`, `WindowMarker`, `Apply`; `curia seen <pane>`; `cmd/curia/hook.go` calls `attention.Apply` after `hook.Run` and a tmux failure never changes the exit code; `TestMarkerByState`, `TestWindowMarkerStrongestWins`, `TestApplySetsWindowOption`, `TestSeenClearsMarker` pass. | merged (PR #9) |

### Wave 5

| Task | Title | Model (expected, confirmed at dispatch) | Done criteria | Status |
| --- | --- | --- | --- | --- |
| 9 | Dashboard popup and sidebar | Opus (confirmed: a UI with keyboard, mouse and layout behaviour) | `ui.Source`, `ui.New`, `Mode`, `ui.ContextLevel`/`Level`; 8 columns, 1 s refresh, the plan's keys; `curia ui` and `curia sidebar`; `TestContextLevelThresholds`, `TestViewShowsRankedRows`, `TestEnterSwitchesToSelectedPane`, `TestSidebarEnterDoesNotQuit`, `TestKillRequiresConfirmation`, `TestFilterNarrowsRows`, `TestRowWithoutPaneNotJumpable`, `TestNarrowWidthTruncates` pass. | merged (PR #11) |

### Wave 6

| Task | Title | Model (expected, confirmed at dispatch) | Done criteria | Status |
| --- | --- | --- | --- | --- |
| 11 | End-to-end replay of five sessions | Opus (confirmed: cross-process test of the built binary with timing and concurrency) | Hand-written fixtures in `testdata/replay/` and golden `e2e/testdata/ls.golden.json` (never generated from program output); `TestReplayFiveSessions` passes (every run exits 0, p95 < 50 ms per process, `ls --json` order S2, S3, S1, S5, `status` prints `W1`); `go test -race ./...` passes; the live check in Step 5 done by hand and reported. **Plan-level criterion:** the orchestrator runs `deadcode ./...` (without `-test`) on `plan-1`; every reported symbol is either a deliberate test double (`tmux.Fake`) or fixed before the PR to main (Ruling 1). | merged (PR #12) |

## Open Questions

1. Resolved by Ruling 4.
2. Go 1.26 is keg-only via Homebrew on the dev Mac and not on PATH by default; owner to add it to their shell config (dotfiles are chezmoi-managed).
3. `TestHookLatency` (Task 5, p95 < 20 ms in-process) runs under `-race` in `make check` and may be flaky, especially on CI runners. Decide at Task 5 dispatch how the budget is measured, without skipping or loosening the test.
4. Resolved by Ruling 5.
5. `go.mod` says `go 1.26.0` plus `toolchain go1.26.11` rather than bare `go 1.26` (decision 0001).
6. Resolved by Ruling 23.
8. `curia uninstall` only removes entries for the current binary path (Ruling 13); entries from a moved or rebuilt binary stay behind and fire against a missing file. Decide whether uninstall should also match older paths.
9. Under sustained heavy write load a waiting SQLite writer can time out (SQLite polls for the lock rather than queueing). Hooks are short so this is unlikely in practice; revisit if the log shows busy errors.
10. Resolved by Ruling 28 for a restarted server. `fleet.Reconcile` matches sessions to panes by pane id alone. A restarted tmux server numbers panes from `%0` again, so an old session whose id is reused is never marked stale and `next` could jump to an unrelated pane. The data model has no server identity; decide in the final review or Plan 2.
11. The `events` table keeps full hook payloads (up to 1 MiB each, including tool output) with no pruning. Add retention in Plan 2.
12. A session outside tmux that dies without SessionEnd keeps its last state forever (it is never stale by design), so a waiting one pins `W1`. An age-out or a `curia forget` belongs in a later plan.
13. Two tmux servers on different sockets can both have a `%1`; telling them apart needs a `tmux_server` column (restore plan).
14. `curia install` records the resolved binary path; an upgrade that changes the path orphans the old hook entries (see Ruling 13). Decide at packaging time whether to match any `curia hook <Event>` entry.
15. SQLite runs with the default `synchronous=FULL`; unmeasured on Linux. Consider `NORMAL` if hook latency is a problem there.
16. A `#` or `%` in the binary path is not escaped for tmux formats in the snippet (hooks in `settings.json` are unaffected). `curia install` should refuse or warn on such a path.
17. A session waiting on a permission prompt is ended and hidden when a background `claude -p` starts in its pane, until its next event (Ruling 27 restores it only then).
18. `SubagentStop` on an ended session is ignored, so `subagents_active` can read high after a resume.
7. No LICENSE file: the owner chose to leave it undecided (2026-11-11).

## Rulings

Format: Ruling: what — why — cost if wrong.

1. Ruling: deadcode runs as `deadcode -test ./...` (test binaries count as roots) — Plan 1 is built bottom-up in parallel waves, so packages like store exist for several waves before any command reaches them; without `-test` every wave-2 PR would fail — a function used only by tests survives until the end; mitigated by the final-wave done criterion above: the orchestrator runs `deadcode ./...` (no `-test`) on plan-1 and every reported symbol must be either a deliberate test double (`tmux.Fake`) or fixed before the PR to main.
2. Ruling: the repo's first commit (spec and plan only) was made directly on main — a PR needs a base branch to exist — none beyond one unreviewed docs commit.
3. Ruling: Phase 1 creates `go.mod` and `cmd/curia/main.go`, although plan Task 1 lists them under "Create"; Task 1 only adds `internal/model` and `internal/config`, so `main.go` already satisfies Task 1's text: a cobra root command named `curia` with a `--version` flag, and nothing else — `go.mod` must exist for setup's dependencies and `main.go` is a shared file owned by the orchestrator — the Task 1 brief needs an amendment.

4. Ruling: Task 7 moves one wave later; waves are now 1: T1; 2: T2, T3, T4, T6, T10; 3: T5; 4: T7, T8; 5: T9; 6: T11 — Tasks 5 and 7 both need config, store and log wiring in `cmd/curia`; Task 5 creates it once and Tasks 7, 8 and 9 reuse it — one extra wave of wall-clock time.
5. Ruling: the plan's Files lists win over "one file per command": `ui` and `sidebar` share `cmd/curia/ui.go`, `install` and `uninstall` share `install.go`; the convention means no command goes in `main.go` or another task's file — the plan decides files — a trivial file split later.
6. Ruling: in Task 10 an entry is ours when its command starts with `<bin> hook ` (bin, one space, `hook`, one space) — the extra spaces in the plan are a formatting artifact — an entry with odd spacing would not be recognised as ours.
7. Ruling: `state.Apply` on a session with an empty ID (first seen mid-run) sets ID, Agent, Pane, Cwd, TranscriptPath and StartedAt from the event, as SessionStart would, then applies the event's own rule — `agent` is NOT NULL in the schema and a row without a pane could never be jumped to — mid-run sessions record a pane where the plan named only the ID.
8. Ruling: an ended session receiving any event except SessionStart is returned unchanged, including `UpdatedAt` — "ignores every event" is the more specific rule — an ended row's `updated_at` does not advance.
9. Ruling: `Session.TmuxWindow` stores the tmux window id (for example `@7`) — Task 8 sets window options by that value — the dashboard must resolve a readable window name another way, decided at Task 9.

10. Ruling: `transcript.Read` takes `LastAssistantText` from the last non-empty text block among all counted assistant lines in the bytes read; model and context still come from the last counted line — Claude Code writes one JSONL line per content block, so the final line is often a tool call with no text — Stop may show text from an earlier message in the same read window.
11. Ruling: when the unread part of a transcript exceeds 1 MiB, `transcript.Read` starts at the first complete line inside the last 1 MiB — the 20 ms hook budget, and resumed sessions have transcripts of tens of MB — a usage line older than the window is missed until the next assistant line.
12. Ruling: the real-tmux test socket is `curia-test-<pid>`, not the plan's fixed `curia-test` — parallel worktrees run `make check` at once and a fixed name lets one run kill another's server — none beyond the literal name.
13. Ruling: Task 10 keeps the plan's rule that only entries starting with the current `<bin> hook ` are ours — the spec says uninstall removes only what it added — stale entries from an old binary path survive an uninstall (Open Question 8).
14. Ruling: the store gains `UpdateSession(ctx, id, fn)`, which runs load, `fn` and upsert in one `BEGIN IMMEDIATE` transaction; every read-modify-write (the hook, `fleet.Reconcile`) uses it, and `UpsertSession` stays the plan's single blind statement — the plan's constraint is "no lost updates" and its own hook flow is a cross-process read-modify-write — hooks serialise on the write lock while `fn` runs; one method beyond the plan's interface.
15. Ruling: store timestamps use a fixed-width UTC layout with 9 fractional digits — `SeenAt` before `UpdatedAt` comparisons lose alerts that land in the same second as a mark-seen — longer text timestamps.

16. Ruling: `curia ls --json` keys are the store's column names for the fields `model.Session` has (`id`, `agent`, `tmux_pane`, `tmux_session`, `tmux_window`, `cwd`, `repo`, `branch`, `state`, `model`, `context_used`, `context_max`, `subagents_active`, `last_update`, `seen_at`, `transcript_path`, `started_at`, `updated_at`); times are RFC 3339 UTC and a zero time is `null`; `transcript_offset` is internal and omitted — the plan says only "snake_case keys" and agents will script against this output — renaming a key later breaks scripts.
17. Ruling: `curia seen` and `curia next` log their errors and exit 0 — tmux runs them from a hook and a key binding, where a failing command prints noise into the status line — a broken install is visible only in the log.
18. Superseded by Ruling 19.
19. Ruling: the hook calls `attention.Apply` only when the event could change the window's marker (the session's marker before and after `hook.Run` differ, or its window changed; a new session ending a red one in the same pane also clears that window), reading the previous row by key — the tmux call measured about 8 ms, against a 20 ms budget on every hook event; this amends the plan's "call `attention.Apply` after `hook.Run`" — a marker change this comparison cannot see is not re-applied until that session's next change or `curia seen`.
20. Ruling: `curia next` reconciles with live panes before choosing its target, as `ls` does; `curia status` does not — `next` must not jump to a dead pane, and `status` runs every 2 seconds — the badge can count a session whose pane was killed until `ls`, `next` or the dashboard runs.
21. Ruling: the dashboard shows `session:window` using the window's name, which it resolves from `tmux.Client.ListPanes` on each refresh, falling back to the stored window id — Ruling 9 stores the id, which is unreadable in a table — one more tmux call per refresh.

22. Ruling: views show the repo as the basename of the stored path; the store and `ls --json` keep the full git top-level path — the plan's hook records `rev-parse --show-toplevel` and the spec's column is `repo@branch` — two repos with the same directory name look alike in a table.
23. Ruling: Task 11 compares `ls --json` with its hand-written golden file after normalising what cannot be fixed in advance: every non-null time becomes `"<time>"` and the repository root in paths becomes `<root>`; payload `cwd` values are paths that do not exist, so repo and branch are empty — the golden file must be written by hand, and real runs produce real times and machine-specific paths — a wrong timestamp value would not be caught by this test (the store tests cover time round-trips).
24. Ruling: Task 11 runs the binary with `TMUX` unset and `TMUX_TMPDIR` set to an empty temp directory, so no tmux server is reachable — the plan says "no tmux server", and the hook would otherwise query the developer's real server — none.
25. Ruling: the live check in Task 11 Step 5 (`curia install`, three real sessions) is left to the owner — it edits the real `~/.claude/settings.json` and `~/.tmux.conf` — Plan 1 merges tested but not proven against a live setup until the owner runs it.

26. Ruling: Task 11 asserts latency (p95 under 50 ms per process, unchanged) over a separate sequential pass of 400 timed runs on a fresh database; the concurrent replay asserts correctness only; one untimed warm-up run is allowed — the script has 10 runs, so its p95 is its maximum, and three simultaneous SessionStarts on a loaded machine measure CPU contention rather than hook cost — latency while several sessions start at the same instant is not gated by a test.

27. Ruling (amends Ruling 8): an ended session resumes on UserPromptSubmit, PreToolUse, PostToolUse, Notification or Stop; SubagentStop and SessionEnd on an ended session are still ignored — a nested `claude -p` in the same pane ends its parent, and the parent would never come back, hiding a session that may be waiting on a permission prompt — a straggler event after a real SessionEnd resurrects a row until its pane closes.
28. Ruling: `tmux.Pane` gains `ServerStart` (from `#{start_time}`), and `fleet.Reconcile` also marks a session stale when it started before the live tmux server did — after a tmux restart pane ids are reused, so `next` and the dashboard's kill would act on unrelated panes (closes Open Question 10) — two servers on different sockets are still not told apart.
29. Ruling (extends Ruling 7): a session whose row does not exist yet gets SessionStart's treatment on its first event of any kind: prior sessions in its pane are ended and repo, branch, tmux session and window are filled — sessions already running at install time otherwise never get a window or a marker — one extra git and tmux call on a session's first event.
30. Ruling (amends the plan's two git commands): SessionStart runs one git process (`rev-parse --show-toplevel --absolute-git-dir`) and reads `HEAD` for the branch — SessionStart took 15 to 23 ms at the median against the 20 ms constraint — branch detection reads git's `HEAD` file format directly.
31. Ruling (amends the spec's F1 table): a Notification whose `notification_type` is `idle_prompt` or `auth_success` changes no state — Claude Code also notifies after about 60 seconds of idle, which would turn every finished session red — if the field differs in the installed Claude Code the old behaviour remains; the owner confirms in the live check.
32. Ruling: the fix wave is one Opus agent on `fix/plan1-final-review` with a PR into `plan-1` and one scoped re-review on Fable — the skill's single fix wave — none.

31a. Ruling (addition to 31): a session first seen on an `idle_prompt` or `auth_success` Notification is recorded as idle with no last update — a row needs a state and idle is what such a session is — none.

## Ledger

One line per task: wave, worktree, branch, status.

- Task 1: wave 1, `.worktrees/plan1-t1-scaffold`, `feat/plan1-t1-scaffold`, merged (PR #2), Sonnet, 0 fix rounds.
- Task 11: wave 6, `.worktrees/plan1-t11-replay`, `feat/plan1-t11-replay`, merged (PR #12), Opus, 1 fix rounds.
- Task 9: wave 5, `.worktrees/plan1-t9-ui`, `feat/plan1-t9-ui`, merged (PR #11), Opus, 1 fix rounds.
- Task 7: wave 4, `.worktrees/plan1-t7-fleet`, `feat/plan1-t7-fleet`, merged (PR #10), Opus, 0 fix rounds.
- Task 8: wave 4, `.worktrees/plan1-t8-attention`, `feat/plan1-t8-attention`, merged (PR #9), Sonnet, 1 fix rounds.
- Task 5: wave 3, `.worktrees/plan1-t5-hook`, `feat/plan1-t5-hook`, merged (PR #8), Opus, 1 fix rounds.
- Task 2: wave 2, `.worktrees/plan1-t2-store`, `feat/plan1-t2-store`, merged (PR #7), Opus, 1 fix rounds.
- Task 3: wave 2, `.worktrees/plan1-t3-state`, `feat/plan1-t3-state`, merged (PR #3), Sonnet, 0 fix rounds.
- Task 4: wave 2, `.worktrees/plan1-t4-transcript`, `feat/plan1-t4-transcript`, merged (PR #4), Sonnet, 1 fix rounds.
- Task 6: wave 2, `.worktrees/plan1-t6-tmux`, `feat/plan1-t6-tmux`, merged (PR #5), Sonnet, 1 fix rounds.
- Task 10: wave 2, `.worktrees/plan1-t10-install`, `feat/plan1-t10-install`, merged (PR #6), Sonnet, 1 fix rounds.

## Next steps

1. Merge `plan-1` into `main` (owner-authorised).
2. Isolated live check; fix what it finds through reviewed PRs.
3. Write Plan 2 (Decisions), merge it as a docs PR, make the shared-file setup changes, execute it in waves on `plan-2`.
4. Owner: run `curia install` on the real machine and the three-session check; add Go 1.26 to PATH.

## Plan 1 results by task

Cost is given as subagent tokens (implementer plus its reviewers, thousands), from the harness's own counts; cost-lens was not run.

| Task | Implementer | Reviewer | Fix rounds | Tokens (k) | Notes |
| --- | --- | --- | --- | --- | --- |
| 1 Scaffold, types, config | Sonnet | Sonnet | 0 | 91 | clean first time |
| 2 Store | Opus | Opus | 1 | 300 | review found the lost-update gap; Rulings 14, 15 |
| 3 State machine | Sonnet | Sonnet | 0 | 95 | clean first time |
| 4 Transcript reader | Sonnet | Sonnet | 1 | 160 | text-across-lines and tail window; Rulings 10, 11 |
| 5 Hook command | Opus | Opus | 1 | 357 | git errors unlogged; test hit the real repo once |
| 6 tmux adapter | Sonnet | Sonnet | 1 | 151 | `%` strftime expansion in display-message |
| 7 Fleet, ls, status, next | Opus (was Sonnet) | Opus | 0 | 232 | clean; pre-review rebase and Ruling 20 |
| 8 Attention markers | Sonnet | Sonnet | 1 | 190 | plus a pre-review change (Ruling 19) |
| 9 Dashboard | Opus | Opus | 1 | 345 | stale rows could be killed |
| 10 Install | Sonnet | Sonnet | 1 | 179 | symlink clobber, backup overwrite |
| 11 End-to-end replay | Opus | Opus | 1 | 354 | blocked once on the latency gate (Ruling 26) |
| Setup (Phase 1) | Opus | orchestrator | 0 | 165 | |
| Final review, fix wave, re-review | Opus fixer | Fable | 1 wave | 712 | 2 Critical, 6 Important, all cross-task |

What this suggests for the next plan: Sonnet was enough for single-package tasks with exact signatures (1, 3, 4, 6, 8, 10), but four of those six needed a fix round for behaviour the plan did not spell out. Every Opus task except 7 also needed one. The findings that mattered most (focus-events, nested sessions, restart ghosts) were cross-task and only the whole-branch review with a live binary found them, so the next plan should run a live smoke check per wave, not only at the end.

## Deferred minors (for the final review)

- Task 1: `config.Default` and `expandHome` discard `os.UserHomeDir` errors; unknown TOML keys are silently ignored.
- Task 2: `TestUpdateSessionLosesNoUpdates` depends on timing (about 2.5x margin); retry `SQLITE_BUSY` inside the test loop. A failed `COMMIT` could return a connection mid-transaction (theoretical).
- Task 3: `SessionStart` on an empty-ID session calls `start` twice; a few untested edges.
- Task 4: first read of a 50 MB transcript measured 16.2 ms warm, close to the 20 ms budget (lever: smaller tail window).
- Task 6: the real-tmux round trip checks `escapeMessage` through raw tmux, not `DisplayMessage` end to end.
- Task 10: a dangling symlink target is still replaced by a regular file; rewritten files get mode 0600 whatever they had; a binary path with a space or quote goes unquoted into the hook command and tmux snippet.
- Task 5: a payload over 1 MiB drops the event; stdin read has no deadline; whole-process hook latency was only measured on a loaded machine (check in Task 11).
- Task 7: `ls --json` HTML-escapes `&&` and passes raw C1 and bidi characters; `ls` help text is inaccurate about hidden stale sessions; `Reconcile`'s in-transaction re-check is untested; `newTmuxClient` is used by four commands and belongs in `common.go`.
- Task 8: each `Apply` re-lists all live sessions; `seen` stops at the first failing window.
- Task 9: `internal/ui` duplicates `ls.go`'s text cleaner and `age`; the kill confirmation does not re-check that the row is still live; the filter matches the full repo path while the cell shows the basename; `ls` table should show the repo basename too (Ruling 22).
- Task 11: SessionStart starts three processes (tmux and two git commands) and takes 15 to 20 ms at the median, close to the 20 ms budget; its tail is the main risk to the e2e latency gate on a loaded machine.
- Final fix wave: a server already carrying the old unmarked badge gets one duplicate on the first re-source after upgrade; a failed keyed read in `hook.Run` drops that event's session update; a watchdog that fires before the log opens exits without a log line.
