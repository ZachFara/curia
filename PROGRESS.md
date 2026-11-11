# PROGRESS

This file mirrors the plan for humans; it does not replace it. Task order, interfaces and tests come from `docs/superpowers/plans/2026-11-11-curia-plan-1-foundation.md`.

## Status

- **Phase 1 (setup): merged** to `main` as PR #1. `main` is protected (PRs required, `check` required, no force pushes, admins included).
- **Phase 2: in progress** on integration branch `plan-1`. Wave 1 merged (Task 1, PR #2). Wave 2 (Tasks 2, 3, 4, 6, 10) dispatched in parallel.

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
| 2 | Store | Opus (confirmed: concurrent writers, WAL and busy_timeout behaviour) | `store.Open` (WAL, `busy_timeout=2000`, embedded migrations recorded in `schema_migrations`), Upsert as one `INSERT ... ON CONFLICT` statement, Get, List with `ListFilter`, AppendEvent, MarkSeen, EndOthersInPane; `0001_init.sql` matches spec section 5 with the two stated differences; `TestOpenCreatesSchemaAndWAL`, `TestOpenIsIdempotent`, `TestUpsertRoundTrip`, `TestListFiltersEndedAndOldStale`, `TestMarkSeen`, `TestEndOthersInPane`, `TestConcurrentUpserts` pass under `-race`. | in progress |
| 3 | State machine | Sonnet (confirmed: one pure function, full rule table in the plan) | `state.Apply` pure (no I/O, no clock), one switch on `ev.Kind`, every rule in the plan's table; `TestApplyTransitions`, `TestEndedIgnoresEventsExceptSessionStart`, `TestSubagentCountNeverNegative`, `TestStopWithOpenDecision`, `TestLastUpdateTruncatedTo80Runes` pass. | merged (PR #3) |
| 4 | Transcript reader | Sonnet (confirmed: one function, exact rules and tests in the plan) | `transcript.Read` with the plan's rules (usage lines only, synthetic and malformed skipped, partial trailing line not consumed, missing file, shrunk file re-read from 0); fixtures in `internal/transcript/testdata/`; `TestReadsLatestUsage`, `TestIncrementalOffset`, `TestPartialTrailingLine`, `TestMissingFile`, `TestShrunkFileRereadsFromZero`, `TestSkipsSyntheticAndMalformed`, `TestLastAssistantTextFirstLine` pass. | in progress |
| 6 | tmux adapter | Sonnet (confirmed: thin exec wrapper with exact signatures; the process boundary is one CLI call per method) | `tmux.Pane`, `tmux.Client` (6 methods), `tmux.NewExec(socket)`, `tmux.Fake` with `Panes` and `Calls`; `TestParseListPanes` passes; `TestExecAgainstRealTmux` runs (not skipped) locally and in CI against `tmux -L curia-test`, and kills its server in `t.Cleanup`. | in progress |
| 10 | Install and uninstall | Sonnet (confirmed: file merge logic with exact rules and tests, no concurrency) | `install.Hooks`, `Unhooks`, `WriteTmux`, `RemoveTmux` per the plan's rules (backup before write, user entries preserved, invalid JSON writes nothing, fence added once); `TestHooksMergePreservesUserHooks`, `TestHooksIdempotent`, `TestHooksBackupWritten`, `TestInvalidJSONUntouched`, `TestMissingSettingsCreated`, `TestUnhooksRemovesOnlyOurs`, `TestTmuxFenceAddedOnceAndRemoved` pass; `curia install` / `uninstall` in `cmd/curia/install.go`. | in progress |

### Wave 3

| Task | Title | Model (expected, confirmed at dispatch) | Done criteria | Status |
| --- | --- | --- | --- | --- |
| 5 | Hook command | Opus (expected, confirmed at dispatch) | `hook.Parse`, `hook.Env`, `hook.Run` in the plan's order; one payload fixture per event kind; `curia hook <Event>` exits 0 on every path and logs errors; `TestParseFixtures`, `TestRunSessionStartRecordsSession`, `TestSessionStartEndsPriorSessionInPane`, `TestHookWithoutTmuxPane`, `TestStopReadsTranscript`, `TestHookLatency` (p95 < 20 ms), `TestMalformedPayloadExitsZero`, `TestHookExitsZeroWhenStoreLocked` pass under `-race`. | not started |

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

## Ledger

One line per task: wave, worktree, branch, status.

- Task 1: wave 1, `.worktrees/plan1-t1-scaffold`, `feat/plan1-t1-scaffold`, merged (PR #2), Sonnet, 0 fix rounds.
- Task 2: wave 2, `.worktrees/plan1-t2-store`, `feat/plan1-t2-store`, in progress.
- Task 3: wave 2, `.worktrees/plan1-t3-state`, `feat/plan1-t3-state`, merged (PR #3), Sonnet, 0 fix rounds.
- Task 4: wave 2, `.worktrees/plan1-t4-transcript`, `feat/plan1-t4-transcript`, in progress.
- Task 6: wave 2, `.worktrees/plan1-t6-tmux`, `feat/plan1-t6-tmux`, in progress.
- Task 10: wave 2, `.worktrees/plan1-t10-install`, `feat/plan1-t10-install`, in progress.

## Next steps

1. Review and merge each Wave 2 task into `plan-1` as it reports.
2. Dispatch Wave 3 (Task 5) once all five are merged.

## Deferred minors (for the final review)

- Task 1: `config.Default` and `expandHome` discard `os.UserHomeDir` errors; unknown TOML keys are silently ignored.
