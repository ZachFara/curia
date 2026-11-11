# PROGRESS

This file mirrors the plan for humans; it does not replace it. Task order, interfaces and tests come from `docs/superpowers/plans/2026-11-11-curia-plan-1-foundation.md`.

## Status

- **Phase 1 (setup): merged** to `main` as PR #1. `main` is protected (PRs required, `check` required, no force pushes, admins included).
- **Phase 2: in progress** on integration branch `plan-1`. Waves 1 and 2 merged (Tasks 1, 2, 3, 4, 6, 10; PRs #2 to #7). Wave 3 (Task 5) dispatched.

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
| 5 | Hook command | Opus (confirmed: integrates four packages, cross-process locking, latency budget) | `hook.Parse`, `hook.Env`, `hook.Run` in the plan's order; one payload fixture per event kind; `curia hook <Event>` exits 0 on every path and logs errors; `TestParseFixtures`, `TestRunSessionStartRecordsSession`, `TestSessionStartEndsPriorSessionInPane`, `TestHookWithoutTmuxPane`, `TestStopReadsTranscript`, `TestHookLatency` (p95 < 20 ms), `TestMalformedPayloadExitsZero`, `TestHookExitsZeroWhenStoreLocked` pass under `-race`. | in progress |

### Wave 4

| Task | Title | Model (expected, confirmed at dispatch) | Done criteria | Status |
| --- | --- | --- | --- | --- |
| 7 | Fleet ranking, badge, ls, status, next | Sonnet (expected, confirmed at dispatch) | `fleet.Reconcile`, `Rank`, `Counts`/`Count`, `Badge`, `Next`; `curia ls [--json]`, `curia status`, `curia next` in their own files; `TestReconcileMarksMissingPanesStale`, `TestReconcileIgnoresSessionsWithoutPane`, `TestRankOrder`, `TestRankOldestFirstWithinGroup`, `TestBadge`, `TestNextSkipsSessionsWithoutPane`, `TestNextNoneWhenAllWorking`, `TestLsJSONGolden` pass. | not started |
| 8 | Attention markers and mark-seen | Sonnet (expected, confirmed at dispatch) | `attention.Marker`, `WindowMarker`, `Apply`; `curia seen <pane>`; `cmd/curia/hook.go` calls `attention.Apply` after `hook.Run` and a tmux failure never changes the exit code; `TestMarkerByState`, `TestWindowMarkerStrongestWins`, `TestApplySetsWindowOption`, `TestSeenClearsMarker` pass. | not started |

### Wave 5

| Task | Title | Model (expected, confirmed at dispatch) | Done criteria | Status |
| --- | --- | --- | --- | --- |
| 9 | Dashboard popup and sidebar | Opus (expected, confirmed at dispatch) | `ui.Source`, `ui.New`, `Mode`, `ui.ContextLevel`/`Level`; 8 columns, 1 s refresh, the plan's keys; `curia ui` and `curia sidebar`; `TestContextLevelThresholds`, `TestViewShowsRankedRows`, `TestEnterSwitchesToSelectedPane`, `TestSidebarEnterDoesNotQuit`, `TestKillRequiresConfirmation`, `TestFilterNarrowsRows`, `TestRowWithoutPaneNotJumpable`, `TestNarrowWidthTruncates` pass. | not started |

### Wave 6

| Task | Title | Model (expected, confirmed at dispatch) | Done criteria | Status |
| --- | --- | --- | --- | --- |
| 11 | End-to-end replay of five sessions | Opus (expected, confirmed at dispatch) | Hand-written fixtures in `testdata/replay/` and golden `e2e/testdata/ls.golden.json` (never generated from program output); `TestReplayFiveSessions` passes (every run exits 0, p95 < 50 ms per process, `ls --json` order S2, S3, S1, S5, `status` prints `W1`); `go test -race ./...` passes; the live check in Step 5 done by hand and reported. **Plan-level criterion:** the orchestrator runs `deadcode ./...` (without `-test`) on `plan-1`; every reported symbol is either a deliberate test double (`tmux.Fake`) or fixed before the PR to main (Ruling 1). | not started |

## Open Questions

1. Resolved by Ruling 4.
2. Go 1.26 is keg-only via Homebrew on the dev Mac and not on PATH by default; owner to add it to their shell config (dotfiles are chezmoi-managed).
3. `TestHookLatency` (Task 5, p95 < 20 ms in-process) runs under `-race` in `make check` and may be flaky, especially on CI runners. Decide at Task 5 dispatch how the budget is measured, without skipping or loosening the test.
4. Resolved by Ruling 5.
5. `go.mod` says `go 1.26.0` plus `toolchain go1.26.11` rather than bare `go 1.26` (decision 0001).
6. Task 11's golden `ls --json` file is hand-written but real runs produce real timestamps; decide at Task 11 dispatch how time fields are compared.
8. `curia uninstall` only removes entries for the current binary path (Ruling 13); entries from a moved or rebuilt binary stay behind and fire against a missing file. Decide whether uninstall should also match older paths.
9. Under sustained heavy write load a waiting SQLite writer can time out (SQLite polls for the lock rather than queueing). Hooks are short so this is unlikely in practice; revisit if the log shows busy errors.
7. No LICENSE file yet although the repo is public; the spec leaves AGPL-3.0 or MIT open. Owner to decide.

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

## Ledger

One line per task: wave, worktree, branch, status.

- Task 1: wave 1, `.worktrees/plan1-t1-scaffold`, `feat/plan1-t1-scaffold`, merged (PR #2), Sonnet, 0 fix rounds.
- Task 5: wave 3, `.worktrees/plan1-t5-hook`, `feat/plan1-t5-hook`, in progress.
- Task 2: wave 2, `.worktrees/plan1-t2-store`, `feat/plan1-t2-store`, merged (PR #7), Opus, 1 fix rounds.
- Task 3: wave 2, `.worktrees/plan1-t3-state`, `feat/plan1-t3-state`, merged (PR #3), Sonnet, 0 fix rounds.
- Task 4: wave 2, `.worktrees/plan1-t4-transcript`, `feat/plan1-t4-transcript`, merged (PR #4), Sonnet, 1 fix rounds.
- Task 6: wave 2, `.worktrees/plan1-t6-tmux`, `feat/plan1-t6-tmux`, merged (PR #5), Sonnet, 1 fix rounds.
- Task 10: wave 2, `.worktrees/plan1-t10-install`, `feat/plan1-t10-install`, merged (PR #6), Sonnet, 1 fix rounds.

## Next steps

1. Review and merge Task 5 into `plan-1`.
2. Dispatch Wave 4 (Tasks 7 and 8).

## Deferred minors (for the final review)

- Task 1: `config.Default` and `expandHome` discard `os.UserHomeDir` errors; unknown TOML keys are silently ignored.
- Task 2: `TestUpdateSessionLosesNoUpdates` depends on timing (about 2.5x margin); retry `SQLITE_BUSY` inside the test loop. A failed `COMMIT` could return a connection mid-transaction (theoretical).
- Task 3: `SessionStart` on an empty-ID session calls `start` twice; a few untested edges.
- Task 4: first read of a 50 MB transcript measured 16.2 ms warm, close to the 20 ms budget (lever: smaller tail window).
- Task 6: the real-tmux round trip checks `escapeMessage` through raw tmux, not `DisplayMessage` end to end.
- Task 10: a dangling symlink target is still replaced by a regular file; rewritten files get mode 0600 whatever they had; a binary path with a space or quote goes unquoted into the hook command and tmux snippet.
