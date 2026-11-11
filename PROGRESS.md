# PROGRESS

This file mirrors the plan for humans; it does not replace it. Task order, interfaces and tests come from `docs/superpowers/plans/2026-11-11-curia-plan-1-foundation.md`.

## Status

- **Phase 1 (setup): in review** on branch `chore/setup`. Module, root command and smoke test, `make check`, lefthook hooks, CI workflow, PR template, AGENTS.md, decision records 0001 to 0009. Waiting for the PR to `main` and the owner's approval.
- GitHub branch protection on `main` (require PRs, require the `check` status, no force pushes) is set up by the orchestrator when the remote exists; it is not in this branch.
- Phase 2 (Plan 1 execution) has not started. It starts only when the owner says so.

## Plan 1 waves

Every task's done criteria also include: `make check` passes in the task's worktree; the PR into `plan-1` uses the PR template; CI (`check`) is green; the task review is clean or every finding is parked with a ruling.

Model column: the expected starting point from the owner's prompt, **expected, confirmed at dispatch**. The orchestrator confirms or changes it, with a one-line reason, before dispatching.

### Wave 1

| Task | Title | Model (expected, confirmed at dispatch) | Done criteria | Status |
| --- | --- | --- | --- | --- |
| 1 | Scaffold, shared types and config | Sonnet (expected, confirmed at dispatch) | `internal/model` types and constants exactly as the Interfaces block; `config.Default`, `config.Load`; `TestDefaults`, `TestLoadMissingFileReturnsDefaults`, `TestLoadOverrides`, `TestCuriaDBEnvOverride` pass; `go build ./cmd/curia` works. `go.mod` and `cmd/curia/main.go` already exist (Ruling 3): Task 1 does not edit them. | not started |

### Wave 2

| Task | Title | Model (expected, confirmed at dispatch) | Done criteria | Status |
| --- | --- | --- | --- | --- |
| 2 | Store | Opus (expected, confirmed at dispatch) | `store.Open` (WAL, `busy_timeout=2000`, embedded migrations recorded in `schema_migrations`), Upsert as one `INSERT ... ON CONFLICT` statement, Get, List with `ListFilter`, AppendEvent, MarkSeen, EndOthersInPane; `0001_init.sql` matches spec section 5 with the two stated differences; `TestOpenCreatesSchemaAndWAL`, `TestOpenIsIdempotent`, `TestUpsertRoundTrip`, `TestListFiltersEndedAndOldStale`, `TestMarkSeen`, `TestEndOthersInPane`, `TestConcurrentUpserts` pass under `-race`. | not started |
| 3 | State machine | Sonnet (expected, confirmed at dispatch) | `state.Apply` pure (no I/O, no clock), one switch on `ev.Kind`, every rule in the plan's table; `TestApplyTransitions`, `TestEndedIgnoresEventsExceptSessionStart`, `TestSubagentCountNeverNegative`, `TestStopWithOpenDecision`, `TestLastUpdateTruncatedTo80Runes` pass. | not started |
| 4 | Transcript reader | Sonnet (expected, confirmed at dispatch) | `transcript.Read` with the plan's rules (usage lines only, synthetic and malformed skipped, partial trailing line not consumed, missing file, shrunk file re-read from 0); fixtures in `internal/transcript/testdata/`; `TestReadsLatestUsage`, `TestIncrementalOffset`, `TestPartialTrailingLine`, `TestMissingFile`, `TestShrunkFileRereadsFromZero`, `TestSkipsSyntheticAndMalformed`, `TestLastAssistantTextFirstLine` pass. | not started |
| 6 | tmux adapter | Sonnet (expected, confirmed at dispatch) | `tmux.Pane`, `tmux.Client` (6 methods), `tmux.NewExec(socket)`, `tmux.Fake` with `Panes` and `Calls`; `TestParseListPanes` passes; `TestExecAgainstRealTmux` runs (not skipped) locally and in CI against `tmux -L curia-test`, and kills its server in `t.Cleanup`. | not started |
| 10 | Install and uninstall | Sonnet (expected, confirmed at dispatch) | `install.Hooks`, `Unhooks`, `WriteTmux`, `RemoveTmux` per the plan's rules (backup before write, user entries preserved, invalid JSON writes nothing, fence added once); `TestHooksMergePreservesUserHooks`, `TestHooksIdempotent`, `TestHooksBackupWritten`, `TestInvalidJSONUntouched`, `TestMissingSettingsCreated`, `TestUnhooksRemovesOnlyOurs`, `TestTmuxFenceAddedOnceAndRemoved` pass; `curia install` / `uninstall` in `cmd/curia/install.go`. | not started |

### Wave 3

| Task | Title | Model (expected, confirmed at dispatch) | Done criteria | Status |
| --- | --- | --- | --- | --- |
| 5 | Hook command | Opus (expected, confirmed at dispatch) | `hook.Parse`, `hook.Env`, `hook.Run` in the plan's order; one payload fixture per event kind; `curia hook <Event>` exits 0 on every path and logs errors; `TestParseFixtures`, `TestRunSessionStartRecordsSession`, `TestSessionStartEndsPriorSessionInPane`, `TestHookWithoutTmuxPane`, `TestStopReadsTranscript`, `TestHookLatency` (p95 < 20 ms), `TestMalformedPayloadExitsZero`, `TestHookExitsZeroWhenStoreLocked` pass under `-race`. | not started |
| 7 | Fleet ranking, badge, ls, status, next | Sonnet (expected, confirmed at dispatch) | `fleet.Reconcile`, `Rank`, `Counts`/`Count`, `Badge`, `Next`; `curia ls [--json]`, `curia status`, `curia next` in their own files; `TestReconcileMarksMissingPanesStale`, `TestReconcileIgnoresSessionsWithoutPane`, `TestRankOrder`, `TestRankOldestFirstWithinGroup`, `TestBadge`, `TestNextSkipsSessionsWithoutPane`, `TestNextNoneWhenAllWorking`, `TestLsJSONGolden` pass. | not started |

### Wave 4

| Task | Title | Model (expected, confirmed at dispatch) | Done criteria | Status |
| --- | --- | --- | --- | --- |
| 8 | Attention markers and mark-seen | Sonnet (expected, confirmed at dispatch) | `attention.Marker`, `WindowMarker`, `Apply`; `curia seen <pane>`; `cmd/curia/hook.go` calls `attention.Apply` after `hook.Run` and a tmux failure never changes the exit code; `TestMarkerByState`, `TestWindowMarkerStrongestWins`, `TestApplySetsWindowOption`, `TestSeenClearsMarker` pass. | not started |
| 9 | Dashboard popup and sidebar | Opus (expected, confirmed at dispatch) | `ui.Source`, `ui.New`, `Mode`, `ui.ContextLevel`/`Level`; 8 columns, 1 s refresh, the plan's keys; `curia ui` and `curia sidebar`; `TestContextLevelThresholds`, `TestViewShowsRankedRows`, `TestEnterSwitchesToSelectedPane`, `TestSidebarEnterDoesNotQuit`, `TestKillRequiresConfirmation`, `TestFilterNarrowsRows`, `TestRowWithoutPaneNotJumpable`, `TestNarrowWidthTruncates` pass. | not started |

### Wave 5

| Task | Title | Model (expected, confirmed at dispatch) | Done criteria | Status |
| --- | --- | --- | --- | --- |
| 11 | End-to-end replay of five sessions | Opus (expected, confirmed at dispatch) | Hand-written fixtures in `testdata/replay/` and golden `e2e/testdata/ls.golden.json` (never generated from program output); `TestReplayFiveSessions` passes (every run exits 0, p95 < 50 ms per process, `ls --json` order S2, S3, S1, S5, `status` prints `W1`); `go test -race ./...` passes; the live check in Step 5 done by hand and reported. **Plan-level criterion:** the orchestrator runs `deadcode ./...` (without `-test`) on `plan-1`; every reported symbol is either a deliberate test double (`tmux.Fake`) or fixed before the PR to main (Ruling 1). | not started |

## Open Questions

1. Wave 3 runs Tasks 5 and 7 in parallel and both add commands that need to load config and open the store in cmd/curia; decide at Phase 2 pre-flight whether one owns a shared helper or Task 7 moves a wave later.
2. Go 1.26 is keg-only via Homebrew on the dev Mac and not on PATH by default; owner to add it to their shell config (dotfiles are chezmoi-managed).
3. `make check` runs every test under `-race`, which slows code several times. `TestHookLatency` (Task 5, p95 < 20 ms in-process) may be flaky under the race detector, especially on CI runners. Decide at Task 5 dispatch how the budget is measured (for example, the test measures without `-race` via a build tag, or the budget is checked against a race-free run), without skipping or loosening the test.
4. The plan puts two commands in one file twice: `cmd/curia/ui.go` holds `ui` and `sidebar` (Task 9), `cmd/curia/install.go` holds `install` and `uninstall` (Task 10). AGENTS.md says one file per command. Proposed reading: the plan's Files list wins, and "one file per command" means no command is added to `main.go` or to another task's file. Confirm before dispatching Wave 2.
5. `go.mod` says `go 1.26.0` plus `toolchain go1.26.11`, not the bare `go 1.26` the setup brief asked for: three dependencies declare `go 1.26.0`, which sorts after `1.26`, so the go command rejects the bare form (decision 0001).

## Rulings

Format: Ruling: what — why — cost if wrong.

1. Ruling: deadcode runs as `deadcode -test ./...` (test binaries count as roots) — Plan 1 is built bottom-up in parallel waves, so packages like store exist for several waves before any command reaches them; without `-test` every wave-2 PR would fail — a function used only by tests survives until the end; mitigated by the Wave 5 done criterion above: the orchestrator runs `deadcode ./...` (no `-test`) on plan-1 and every reported symbol must be either a deliberate test double (`tmux.Fake`) or fixed before the PR to main.
2. Ruling: the repo's first commit (spec and plan only) was made directly on main — a PR needs a base branch to exist — none beyond one unreviewed docs commit.
3. Ruling: Phase 1 creates `go.mod` and `cmd/curia/main.go`, although plan Task 1 lists them under "Create"; Task 1 only adds `internal/model` and `internal/config`, so `main.go` already satisfies Task 1's text: a cobra root command named `curia` with a `--version` flag, and nothing else — `go.mod` must exist for setup's dependencies and `main.go` is a shared file owned by the orchestrator — the Task 1 brief needs an amendment.

## Ledger

One line per task: wave, worktree, branch, status. Empty until Phase 2.

## Next steps

1. Owner reviews and approves the `chore/setup` PR; orchestrator sets up branch protection on `main` requiring the `check` status.
2. After merge: create `plan-1` from `main`.
3. Phase 2 pre-flight: verify the waves against each task's Interfaces block, settle Open Questions 1, 3 and 4, confirm each task's model, and write Task 1's brief with the Ruling 3 amendment.
4. Dispatch Wave 1 (Task 1).
