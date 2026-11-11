# Curia Implementation Plan 1: Foundation

Nov 11, 2026 · @Zachariah Farahany

## TLDR

Plan 1 builds the part of Curia every later feature reads from: a Go binary whose Claude Code hooks write live session state into SQLite, plus the views over it (status badge, window markers, jump-to-next, and the dashboard popup). It ships in 11 test-first tasks and is done when five parallel sessions show the correct live state. Decisions, tasks, review, remote and cost come in Plans 2 to 5, each building on this store.

## Contents

1. Plan sequence
2. Global constraints
3. Review focus
4. File structure
5. Tasks 1 to 4
6. Tasks 5 to 8
7. Tasks 9 to 11

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** a `curia` binary that tracks every Claude Code session in tmux and shows who needs me, from the status bar or a popup, within 1 second of any change.

**Architecture:** Claude Code hooks call `curia hook <Event>`, which parses the payload, applies a pure state machine and upserts one row per session in SQLite (WAL). Every view (`status`, `next`, `ls`, `ui`) reads that store; nothing scrapes panes. tmux is driven through a small adapter interface so all logic is testable without tmux.

**Tech stack:** Go 1.26, `modernc.org/sqlite` (pure Go, no cgo), `github.com/spf13/cobra`, `github.com/charmbracelet/bubbletea` and `lipgloss`, `github.com/BurntSushi/toml`.

**Spec:** [Curia: V1 Feature Spec](https://claude.ai/artifact/6RctpvCtAtAZGwQt9aYW4V). This plan implements F1 to F4 and the data model's `sessions` and `events` tables. Read the spec before starting.

## 1. Plan sequence

The spec has 18 features across several independent subsystems, so it is split into five plans. Each ships working, testable software and adds tables to the store through numbered migrations. Only Plan 1 is written in detail; each later plan gets its own document once the previous one ships, so it can follow the code as built.

| Plan | Features | Depends on | Done when |
| --- | --- | --- | --- |
| 1. Foundation (this doc) | F1 to F4 | nothing | 5 parallel sessions show correct live state in `curia ls`, the badge and the popup |
| 2. Decisions | F7 to F9, F17 | Plan 1 | an agent raises a ticket, I answer in the popup, the agent continues; precedent search returns past answers |
| 3. Tasks, spawning and teams | F5, F6, F14, F18 | Plans 1, 2 | `curia spawn --team T-42` opens a lead in its own worktree; teammate tickets are routed to the lead; `curia say` routes a plain-language message to the right agent |
| 4. Review, remote and restore | F10 to F12 | Plans 1 to 3 | a finished task appears in the review queue; a blocking ticket reaches my phone; `curia restore` rebuilds a killed tmux server |
| 5. Cost and integrations | F13, F15, F16 | Plans 1, 2 | cost-lens pushes into `curia ingest`; the badge forecasts time to limit; decisions export to Obsidian |

F17 (cache-aware triage) ships with Plan 2 with its sort and display, and shows real values once Plan 5 supplies cache state. Two pieces of F1 to F4 also wait for later plans: the session cost column (Plan 5, from cost-lens) and the bell or macOS notification on a new blocking decision (Plan 2).

## 2. Global constraints

Every task's requirements include these.

- Go 1.26; pure-Go dependencies only (no cgo), so `GOOS=linux` and `GOOS=darwin` builds work from either machine.
- Hook execution adds under 20 ms per event (spec F1).
- `curia hook` always exits 0, whatever goes wrong; errors go to the log file, never to Claude Code.
- A session started in any pane appears in the store within 1 second (spec F1).
- Stale sessions are hidden 10 minutes after their pane disappears (spec F1).
- Context colors: green under 60%, yellow 60 to 85%, red above 85% (spec F2).
- Status badge format `D<n> W<n> R<n>`, zero parts omitted, empty string when all are zero (spec F4).
- tmux 3.2 or later (needed for `display-popup`).
- Paths: database `~/.local/share/curia/curia.db`, log `~/.local/state/curia/curia.log`, config `~/.config/curia/config.toml`; `CURIA_DB` overrides the database path (used by tests).
- SQLite in WAL mode with `busy_timeout` of 2000 ms.
- `curia install` backs up `~/.claude/settings.json` before writing and never removes entries it did not add (spec section 7).
- No network access anywhere in Plan 1.

## 3. Review focus

Five conditions the spec implies but no feature names, most likely to bite first. Each has a test in the task that owns the code.

1. **Parallel hooks.** Several sessions fire hooks at the same instant. Expected: no lost updates and no failed hook; a write that waits out `busy_timeout` is logged and the hook still exits 0. Tested in Task 2 (`TestConcurrentUpserts`) and Task 5 (`TestHookExitsZeroWhenStoreLocked`).
2. **Hook outside tmux.** `$TMUX_PANE` is empty (Claude Code run in a plain terminal). Expected: the session is recorded with no pane, listed, but not jumpable and never marked stale. Tested in Task 5 (`TestHookWithoutTmuxPane`) and Task 7 (`TestNextSkipsSessionsWithoutPane`).
3. **Transcript edge cases.** The transcript ends in a half-written line, is missing, or shrinks (rewritten). Expected: no crash, offset stays at the last complete line, a shrunken file is re-read from 0. Tested in Task 4.
4. **Hostile settings.json.** The user's file has their own hooks, or is invalid JSON. Expected: their hooks survive install and uninstall untouched; on invalid JSON, install aborts with an error and changes nothing. Tested in Task 10.
5. **Reused pane.** A new session starts in a pane that still has an old session bound to it (agent restarted in place, or tmux server restarted and ids recycled). Expected: the old session becomes `ended`, so the pane maps to one live session. Tested in Task 2 (`TestEndOthersInPane`) and Task 5 (`TestSessionStartEndsPriorSessionInPane`).

## 4. File structure

One Go module, `github.com/ZachFara/curia`. Each package has one job and depends only on packages above it in this table, so every package below `model` can be tested alone.

| Path | Responsibility | Task |
| --- | --- | --- |
| `internal/model/` | Shared types: `Session`, `State`, `Event`, `EventKind`. No logic, no imports beyond the standard library. | 1 |
| `internal/config/` | Load `config.toml` over defaults; expand `~`; `CURIA_DB` override. | 1 |
| `internal/store/` | SQLite: open, migrate, upsert and list sessions, append events, mark seen. Schema in `migrations/0001_init.sql`, embedded. | 2 |
| `internal/state/` | Pure state machine: `Apply(session, event, openDecision) session`. No I/O. | 3 |
| `internal/transcript/` | Incremental JSONL reader: model, context used, last assistant text. | 4 |
| `internal/hook/` | Parse a hook payload into an `Event`; run one hook end to end. | 5 |
| `internal/tmux/` | `Client` interface over the tmux CLI, an exec implementation, a fake for tests. | 6 |
| `internal/fleet/` | Reconcile with live panes, rank sessions, count, badge, next target. Pure except `Reconcile`. | 7 |
| `internal/attention/` | Window markers and mark-seen, applied through `tmux.Client`. | 8 |
| `internal/ui/` | Bubble Tea dashboard model and view. | 9 |
| `internal/install/` | Merge and remove hooks in `settings.json`; write the tmux snippet. | 10 |
| `cmd/curia/` | Cobra commands wiring the packages: `hook`, `ls`, `status`, `next`, `seen`, `ui`, `install`, `uninstall`. | 1, 5, 7, 8, 9, 10 |
| `testdata/replay/` | Recorded hook payloads for five parallel sessions. | 11 |
| `e2e/` | Builds the binary and replays `testdata/replay/`. | 11 |

## 5. Tasks 1 to 4

### Task 1: Scaffold, shared types and config

**Files:**

- Create: `go.mod`, `cmd/curia/main.go`, `internal/model/model.go`, `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**

- Consumes: nothing.
- Produces:
  - `model.State` (string) with `StateWorking = "working"`, `StateWaiting = "waiting"`, `StateNeedsDecision = "needs_decision"`, `StateIdle = "idle"`, `StateEnded = "ended"`, `StateStale = "stale"`.
  - `model.EventKind` (string) with one constant per Claude Code hook name: `SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `Notification`, `Stop`, `SubagentStop`, `SessionEnd`.
  - `model.Session{ID, Agent, Pane, TmuxSession, TmuxWindow, Cwd, Repo, Branch, Model, LastUpdate, TranscriptPath string; State State; ContextUsed, ContextMax, SubagentsActive int; TranscriptOffset int64; StartedAt, UpdatedAt, SeenAt time.Time}`
  - `model.Event{Kind EventKind; SessionID, Pane, Cwd, TranscriptPath, ToolName, ToolTarget, Message string; At time.Time}`
  - `config.Config{DBPath, LogPath string; ContextMax map[string]int; DefaultContextMax, ContextWarnPct, ContextCritPct int; StaleHideAfter time.Duration}`
  - `config.Default() Config` and `config.Load(path string) (Config, error)`

* [ ] **Step 1: Write the failing tests**

```go
func TestDefaults(t *testing.T) {
    c := config.Default()
    // DBPath ends with ".local/share/curia/curia.db", LogPath with ".local/state/curia/curia.log"
    // DefaultContextMax == 200000, ContextWarnPct == 60, ContextCritPct == 85
    // StaleHideAfter == 10*time.Minute
}
func TestLoadMissingFileReturnsDefaults(t *testing.T) // Load("/nonexistent") == Default(), err == nil
func TestLoadOverrides(t *testing.T)
// toml: context_warn_pct = 50 and [context_max] "claude-opus-5-5" = 1000000
// -> ContextWarnPct == 50, ContextMax["claude-opus-5-5"] == 1000000, other fields default
func TestCuriaDBEnvOverride(t *testing.T) // t.Setenv("CURIA_DB", dir+"/x.db") -> DBPath == that path
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/config/ -v`. Expected: FAIL, package does not compile.

- [ ] **Step 3: Implement** `internal/model/model.go` (types only), `config.Default` and `config.Load` (BurntSushi/toml decoded over `Default()`, `~` expanded with `os.UserHomeDir`, `CURIA_DB` applied last), and `cmd/curia/main.go` with a cobra root command named `curia` and a `--version` flag.
- [ ] **Step 4: Run and confirm they pass.** Run: `go test ./internal/config/ -v && go build ./cmd/curia`. Expected: PASS, binary builds.
- [ ] **Step 5: Commit.** `git commit -am "feat: scaffold module, shared types and config"`

### Task 2: Store

**Files:**

- Create: `internal/store/store.go`, `internal/store/migrations/0001_init.sql`
- Test: `internal/store/store_test.go`

**Interfaces:**

- Consumes: `model.Session`, `model.Event`, `model.State`.
- Produces:
  - `store.Open(path string) (*Store, error)`: creates the parent directory, sets `journal_mode=WAL` and `busy_timeout=2000`, applies embedded migrations in filename order, recording each in a `schema_migrations` table.
  - `(*Store) Close() error`
  - `(*Store) UpsertSession(ctx context.Context, s model.Session) error`
  - `(*Store) GetSession(ctx context.Context, id string) (model.Session, bool, error)`
  - `(*Store) ListSessions(ctx context.Context, f ListFilter) ([]model.Session, error)` with `ListFilter{IncludeEnded bool; HideStaleBefore time.Time}`: excludes `ended` unless asked, and excludes `stale` rows whose `UpdatedAt` is before `HideStaleBefore`.
  - `(*Store) AppendEvent(ctx context.Context, ev model.Event, raw []byte) error`
  - `(*Store) MarkSeen(ctx context.Context, pane string, at time.Time) error`: sets `SeenAt` on every non-ended session in that pane.
  - `(*Store) EndOthersInPane(ctx context.Context, pane, keepID string, at time.Time) error`: sets every other non-ended session in that pane to `ended`.

`0001_init.sql` creates `sessions` and `events` exactly as spec section 5, with two differences: `task_id` is plain `TEXT` (the `tasks` table arrives in Plan 3), and `sessions` gains `transcript_offset INTEGER NOT NULL DEFAULT 0`.

- [ ] **Step 1: Write the failing tests**

```go
func TestOpenCreatesSchemaAndWAL(t *testing.T)   // PRAGMA journal_mode returns "wal"; sessions and events exist
func TestOpenIsIdempotent(t *testing.T)          // Open twice on one path; schema_migrations has 1 row
func TestUpsertRoundTrip(t *testing.T)           // every Session field survives Upsert then Get, times to the second
func TestListFiltersEndedAndOldStale(t *testing.T)
// rows: working, ended, stale updated 11m ago, stale updated 2m ago; HideStaleBefore = now-10m
// -> returns working and the 2m stale only
func TestMarkSeen(t *testing.T)                  // two sessions in %3, one ended: only the live one gets SeenAt
func TestEndOthersInPane(t *testing.T)           // A and B in %3, keep B -> A ended, B unchanged
func TestConcurrentUpserts(t *testing.T)
// 5 Store handles on one file, 20 goroutines x 50 upserts over 5 session ids
// -> every call returns nil; ListSessions returns exactly 5 rows
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/store/ -v`. Expected: FAIL, undefined `store.Open`.
- [ ] **Step 3: Implement** the functions above with `modernc.org/sqlite` and `embed.FS`. Upsert is one `INSERT ... ON CONFLICT(id) DO UPDATE` statement, so concurrent hooks never read-modify-write.
- [ ] **Step 4: Run and confirm they pass,** including under the race detector. Run: `go test -race ./internal/store/ -v`. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -am "feat: sqlite store with sessions and events"`

### Task 3: State machine

**Files:**

- Create: `internal/state/state.go`
- Test: `internal/state/state_test.go`

**Interfaces:**

- Consumes: `model.Session`, `model.Event`.
- Produces: `state.Apply(s model.Session, ev model.Event, openDecision bool) model.Session`. Pure: no I/O, no clock.

Rules (spec F1 table; a session first seen mid-run, with no SessionStart, takes its ID from the event): every event sets `UpdatedAt = ev.At`. An `ended` session ignores every event except `SessionStart`.

| Event | State | Also sets |
| --- | --- | --- |
| `SessionStart` | working | `ID`, `Agent = "claude"`, `Pane`, `Cwd`, `TranscriptPath`, `StartedAt`; `SubagentsActive = 0` |
| `UserPromptSubmit` | working | `SeenAt = ev.At` |
| `PreToolUse` | working | `LastUpdate`; `SubagentsActive++` when `ToolName` is `Task` or `Agent` |
| `PostToolUse` | working | `LastUpdate` |
| `Notification` | waiting | `LastUpdate = Message` |
| `Stop` | idle, or needs\_decision when `openDecision` | `LastUpdate = Message` when non-empty |
| `SubagentStop` | unchanged | `SubagentsActive--`, floor 0 |
| `SessionEnd` | ended |  |

`LastUpdate` for tool events is `ToolName + " " + ToolTarget` (just `ToolName` when the target is empty). Every `LastUpdate` is cut to 80 runes.

- [ ] **Step 1: Write the failing tests**

```go
func TestApplyTransitions(t *testing.T)   // table-driven, one row per table row above, asserting State and each "Also sets" field
func TestEndedIgnoresEventsExceptSessionStart(t *testing.T)
func TestSubagentCountNeverNegative(t *testing.T) // SubagentStop on 0 -> 0
func TestStopWithOpenDecision(t *testing.T)       // -> StateNeedsDecision
func TestLastUpdateTruncatedTo80Runes(t *testing.T) // 100-rune target with multibyte runes -> 80 runes
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/state/ -v`. Expected: FAIL, undefined `state.Apply`.
- [ ] **Step 3: Implement `state.Apply`** as one switch on `ev.Kind`.
- [ ] **Step 4: Run and confirm they pass.** Same command. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -am "feat: session state machine"`

### Task 4: Transcript reader

**Files:**

- Create: `internal/transcript/transcript.go`, `internal/transcript/testdata/*.jsonl`
- Test: `internal/transcript/transcript_test.go`

**Interfaces:**

- Consumes: nothing.
- Produces: `transcript.Read(path string, offset int64) (Update, int64, error)` with `Update{Model string; ContextUsed int; LastAssistantText string; Found bool}`. The returned offset is the end of the last complete line read.

Rules: only lines with `"type":"assistant"` and a `message.usage` object count; the last such line in the new bytes wins. `ContextUsed = input_tokens + cache_read_input_tokens + cache_creation_input_tokens`. Lines whose `message.model` is `<synthetic>` are skipped. `LastAssistantText` is the first line of the last `text` content block. Malformed complete lines are skipped. A trailing line without `\n` is not consumed. A missing file returns `(Update{}, offset, nil)`. A file shorter than `offset` is re-read from 0.

- [ ] **Step 1: Write the failing tests**

```go
func TestReadsLatestUsage(t *testing.T)          // two assistant lines -> ContextUsed and Model from the second; Found == true
func TestIncrementalOffset(t *testing.T)         // read, append one line, read from returned offset -> sees only the new line
func TestPartialTrailingLine(t *testing.T)       // last line lacks "\n" -> offset stops before it; completing it later reads it
func TestMissingFile(t *testing.T)               // -> Update{}, same offset, nil error
func TestShrunkFileRereadsFromZero(t *testing.T) // offset 10000 on a 300-byte file -> reads from 0
func TestSkipsSyntheticAndMalformed(t *testing.T)
func TestLastAssistantTextFirstLine(t *testing.T) // "Done.\nDetails..." -> "Done."
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/transcript/ -v`. Expected: FAIL, undefined `transcript.Read`.
- [ ] **Step 3: Implement `transcript.Read`** with `os.Open`, `Seek(offset)` and a `bufio.Reader` reading whole lines.
- [ ] **Step 4: Run and confirm they pass.** Same command. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -am "feat: incremental transcript reader"`

## 6. Tasks 5 to 8

### Task 5: Hook command

**Files:**

- Create: `internal/hook/parse.go`, `internal/hook/run.go`, `internal/hook/testdata/*.json` (one payload per event kind), `cmd/curia/hook.go`
- Test: `internal/hook/hook_test.go`, `cmd/curia/hook_test.go`

**Interfaces:**

- Consumes: `store` (Task 2), `state.Apply` (Task 3), `transcript.Read` (Task 4), `config.Config` (Task 1).
- Produces:
  - `hook.Parse(kind model.EventKind, payload []byte, pane string, now time.Time) (model.Event, error)`. Reads `session_id`, `cwd`, `transcript_path`, `tool_name`, `message`; `ToolTarget` is the first present of `tool_input.file_path`, `tool_input.pattern`, or `tool_input.command` cut to 60 runes.
  - `hook.Env{Pane string; Now func() time.Time; Git func(cwd string) (repo, branch string); PaneInfo func(pane string) (tmuxSession, window string)}`
  - `hook.Run(ctx context.Context, kind model.EventKind, payload []byte, env Env, st *store.Store, cfg config.Config) (model.Session, error)`

`Run` in order: parse; append the raw event; load the session; on `SessionStart` with a pane, call `EndOthersInPane` and fill `Repo`, `Branch`, `TmuxSession`, `TmuxWindow` from `env`; on `PostToolUse`, `Notification` and `Stop`, call `transcript.Read` from `TranscriptOffset` and copy `Model`, `ContextUsed`, the new offset, and `ContextMax` (`cfg.ContextMax[model]`, else `cfg.DefaultContextMax`); on `Stop`, set `ev.Message` to `LastAssistantText`; apply the state machine with `openDecision = false` (Plan 2 wires real decisions); upsert.

`curia hook <Event>` reads stdin and `$TMUX_PANE`, opens the store, calls `Run`, writes any error to the log file, and calls `os.Exit(0)` on every path.

- [ ] **Step 1: Write the failing tests**

```go
func TestParseFixtures(t *testing.T)                // each testdata payload -> expected Event fields
func TestRunSessionStartRecordsSession(t *testing.T) // -> working, Pane "%3", Repo and Branch from fake Git
func TestSessionStartEndsPriorSessionInPane(t *testing.T) // A started in %3, then B in %3 -> A ended, B working
func TestHookWithoutTmuxPane(t *testing.T)          // Pane "" -> session stored with empty Pane, no EndOthersInPane call
func TestStopReadsTranscript(t *testing.T)          // -> idle, Model and ContextUsed from fixture, LastUpdate = first text line
func TestHookLatency(t *testing.T)                  // 200 PostToolUse runs on a warm store: p95 < 20ms
// cmd/curia/hook_test.go, running the hook command in-process with a fake exit func:
func TestMalformedPayloadExitsZero(t *testing.T)    // payload "{" -> exit code 0, one line in the log
func TestHookExitsZeroWhenStoreLocked(t *testing.T) // another connection holds BEGIN EXCLUSIVE -> exit 0 within 3s, error logged
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/hook/ ./cmd/curia/ -v`. Expected: FAIL, undefined `hook.Run`.
- [ ] **Step 3: Implement** `Parse`, `Run` and the `hook` cobra command. `env.Git` runs `git -C <cwd> rev-parse --show-toplevel` and `git -C <cwd> branch --show-current`, and only on `SessionStart`, to stay inside the latency budget.
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/hook/ ./cmd/curia/ -v`. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -am "feat: curia hook command"`

### Task 6: tmux adapter

**Files:**

- Create: `internal/tmux/tmux.go` (interface and `Pane`), `internal/tmux/exec.go`, `internal/tmux/fake.go`
- Test: `internal/tmux/tmux_test.go`

**Interfaces:**

- Consumes: nothing.
- Produces:
  - `tmux.Pane{ID, Session, WindowID, WindowName string}`
  - `tmux.Client` interface: `ListPanes(ctx) ([]Pane, error)`, `PaneInfo(ctx, pane string) (Pane, error)`, `SwitchTo(ctx, pane string) error`, `SetWindowOption(ctx, window, name, value string) error`, `DisplayMessage(ctx, text string) error`, `KillPane(ctx, pane string) error`
  - `tmux.NewExec(socket string) Client` (empty socket = default server; otherwise passes `-L socket`)
  - `tmux.Fake` implementing `Client`, with settable `Panes` and a `Calls []string` log

`ListPanes` runs `tmux list-panes -a -F '#{pane_id}\t#{session_name}\t#{window_id}\t#{window_name}'`. `SwitchTo` runs `switch-client -t <pane>`.

- [ ] **Step 1: Write the failing tests**

```go
func TestParseListPanes(t *testing.T) // 3 lines incl. a window name with spaces -> 3 Panes; trailing newline ignored
func TestExecAgainstRealTmux(t *testing.T)
// skipped when tmux is not on PATH; starts `tmux -L curia-test new-session -d -s t`,
// ListPanes returns 1 pane in session "t", SetWindowOption then show-options reads it back; kills the server in t.Cleanup
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/tmux/ -v`. Expected: FAIL.
- [ ] **Step 3: Implement** the interface, the exec client (`exec.CommandContext`), and the fake.
- [ ] **Step 4: Run and confirm they pass.** Same command. Expected: PASS (the real-tmux test runs where tmux is installed).
- [ ] **Step 5: Commit.** `git commit -am "feat: tmux adapter"`

### Task 7: Fleet ranking, badge and the ls, status and next commands

**Files:**

- Create: `internal/fleet/fleet.go`, `cmd/curia/ls.go`, `cmd/curia/status.go`, `cmd/curia/next.go`
- Test: `internal/fleet/fleet_test.go`, `cmd/curia/ls_test.go`

**Interfaces:**

- Consumes: `store` (Task 2), `tmux.Client` (Task 6).
- Produces:
  - `fleet.Reconcile(ctx context.Context, st *store.Store, live []tmux.Pane, now time.Time) error`: non-ended sessions with a non-empty `Pane` that is not live become `stale` with `UpdatedAt = now`.
  - `fleet.Rank(s []model.Session) []model.Session`: groups in order needs\_decision, waiting, idle-unseen (`SeenAt` before `UpdatedAt`), working, idle-seen, stale; oldest `UpdatedAt` first within a group. The spec's review-ready group slots in after waiting in Plan 4.
  - `fleet.Counts{Decisions, Waiting, Reviews int}`, `fleet.Count(s []model.Session) Counts` (Reviews stays 0 until Plan 4)
  - `fleet.Badge(c Counts) string`
  - `fleet.Next(s []model.Session) (model.Session, bool)`: the first ranked session that has a pane and is needs\_decision, waiting or idle-unseen.

`curia ls [--json]` reconciles (skipped with a log line when tmux is unreachable), lists with `HideStaleBefore = now - cfg.StaleHideAfter`, ranks and prints a table or a JSON array with snake\_case keys. `curia status` prints the badge with no newline, and prints nothing on any error. `curia next` switches to `fleet.Next`, or shows `nothing needs you` through `DisplayMessage`.

- [ ] **Step 1: Write the failing tests**

```go
func TestReconcileMarksMissingPanesStale(t *testing.T)
func TestReconcileIgnoresSessionsWithoutPane(t *testing.T) // empty Pane is never marked stale
func TestRankOrder(t *testing.T)                         // one session per group, shuffled in -> groups in order
func TestRankOldestFirstWithinGroup(t *testing.T)
func TestBadge(t *testing.T) // {2,1,1} -> "D2 W1 R1"; {0,1,0} -> "W1"; {0,0,0} -> ""
func TestNextSkipsSessionsWithoutPane(t *testing.T)
func TestNextNoneWhenAllWorking(t *testing.T)
func TestLsJSONGolden(t *testing.T) // seeded temp DB via CURIA_DB -> output equals testdata/ls.golden.json
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/fleet/ ./cmd/curia/ -v`. Expected: FAIL.
- [ ] **Step 3: Implement** the fleet functions and the three commands.
- [ ] **Step 4: Run and confirm they pass.** Same command. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -am "feat: fleet ranking, badge, ls, status and next"`

### Task 8: Attention markers and mark-seen

**Files:**

- Create: `internal/attention/attention.go`, `cmd/curia/seen.go`
- Modify: `cmd/curia/hook.go` (call `attention.Apply` after `hook.Run`)
- Test: `internal/attention/attention_test.go`

**Interfaces:**

- Consumes: `store` (Task 2), `tmux.Client` (Task 6), `hook.Run`'s returned session (Task 5).
- Produces:
  - `attention.Marker(s model.Session) string`: `"red"` when the state is needs\_decision or waiting and `SeenAt` is before `UpdatedAt`; `""` otherwise. Plan 4 adds `"blue"` for review-ready.
  - `attention.WindowMarker(s []model.Session) string`: the strongest marker among sessions in one window.
  - `attention.Apply(ctx context.Context, tm tmux.Client, st *store.Store, window string) error`: sets the tmux window option `@curia_marker` to `WindowMarker` of the window's live sessions.

`curia seen <pane>` calls `MarkSeen` and then `Apply` for that pane's window. The tmux snippet (Task 10) wires it to `pane-focus-in` and reads `#{@curia_marker}` in `window-status-format`.

- [ ] **Step 1: Write the failing tests**

```go
func TestMarkerByState(t *testing.T)          // waiting unseen -> "red"; waiting seen -> ""; working -> ""
func TestWindowMarkerStrongestWins(t *testing.T) // working + waiting-unseen in one window -> "red"
func TestApplySetsWindowOption(t *testing.T)  // fake records SetWindowOption("@7", "@curia_marker", "red")
func TestSeenClearsMarker(t *testing.T)       // waiting session, then seen -> SetWindowOption("@7", "@curia_marker", "")
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/attention/ -v`. Expected: FAIL.
- [ ] **Step 3: Implement** the three functions, the `seen` command, and the call in the `hook` command. A tmux failure there is logged and never changes the hook's exit code.
- [ ] **Step 4: Run and confirm they pass.** Run: `go test ./internal/attention/ ./cmd/curia/ -v`. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -am "feat: window attention markers and mark-seen"`

## 7. Tasks 9 to 11

### Task 9: Dashboard popup and sidebar

**Files:**

- Create: `internal/ui/model.go`, `internal/ui/view.go`, `cmd/curia/ui.go`
- Test: `internal/ui/ui_test.go`

**Interfaces:**

- Consumes: `fleet.Rank`, `fleet.Reconcile` (Task 7), `tmux.Client` (Task 6), `config.Config` (Task 1).
- Produces:
  - `ui.Source` interface: `Sessions(ctx context.Context) ([]model.Session, error)`; the command's implementation reconciles, lists and ranks.
  - `ui.New(src Source, tm tmux.Client, cfg config.Config, now func() time.Time, mode Mode) Model` (a `tea.Model`), with `Mode` either `ModePopup` or `ModeSidebar`.
  - `ui.ContextLevel(used, max, warnPct, critPct int) Level`, with `Level` one of `LevelUnknown`, `LevelOK`, `LevelWarn`, `LevelCrit`.

Columns in Plan 1: state icon, `session:window`, `repo@branch`, model, context %, subagents, last update, age of last update. Task, decisions and cost columns arrive with Plans 2, 3 and 5. The model re-reads `Source` every second.

Keys: `j`/`k` or arrows move; `Enter` or a left click jumps (`SwitchTo`), then quits in popup mode and stays open in sidebar mode; `/` filters on repo, branch and last update; `x` then `y` kills the pane, any other key cancels; `q` or `Esc` quits. The `d`, `r`, `t` and `n` keys belong to later plans.

`curia ui` runs popup mode in the alternate screen. `curia sidebar` runs sidebar mode with the narrow layout (state, window, context, age).

- [ ] **Step 1: Write the failing tests**

```go
func TestContextLevelThresholds(t *testing.T) // 59% OK, 60% Warn, 85% Warn, 86% Crit, max 0 Unknown
func TestViewShowsRankedRows(t *testing.T)    // fake Source, 3 sessions -> View lists them in Source order with all 8 columns
func TestEnterSwitchesToSelectedPane(t *testing.T) // move down, Enter -> fake records SwitchTo("%5"), returns tea.Quit
func TestSidebarEnterDoesNotQuit(t *testing.T)
func TestKillRequiresConfirmation(t *testing.T) // "x" alone -> no KillPane; "x","y" -> KillPane("%5")
func TestFilterNarrowsRows(t *testing.T)       // "/", "auth" -> only rows whose repo, branch or update contains "auth"
func TestRowWithoutPaneNotJumpable(t *testing.T) // Enter -> no SwitchTo; status line reads "not in tmux"
func TestNarrowWidthTruncates(t *testing.T)    // WindowSizeMsg width 60 -> no rendered line wider than 60 cells
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/ui/ -v`. Expected: FAIL.
- [ ] **Step 3: Implement** the model with Bubble Tea and lipgloss, and the `ui` and `sidebar` commands. Colors: green, yellow and red for the three context levels, dim for stale rows.
- [ ] **Step 4: Run and confirm they pass.** Same command. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -am "feat: dashboard popup and sidebar"`

### Task 10: Install and uninstall

**Files:**

- Create: `internal/install/settings.go`, `internal/install/tmuxconf.go`, `cmd/curia/install.go`
- Test: `internal/install/install_test.go`

**Interfaces:**

- Consumes: nothing from earlier tasks.
- Produces:
  - `install.Hooks(settingsPath, bin string, now time.Time) error` and `install.Unhooks(settingsPath, bin string) error`
  - `install.WriteTmux(snippetPath, tmuxConfPath, bin string) error` and `install.RemoveTmux(tmuxConfPath string) error`

Hooks rules: for each of the 8 event kinds, add `{"hooks": [{"type": "command", "command": "<bin> hook <Event>"}]}` to `settings["hooks"][<Event>]`, with `"matcher": "*"` on `PreToolUse` and `PostToolUse`. An entry is ours when its command starts with ` <bin> hook  `. Adding is idempotent. Before any write, copy the file to `settings.json.bak.<unix seconds>`. Every other key and entry is preserved. Invalid JSON returns an error and writes nothing. A missing file is created.

The tmux snippet, written to `~/.config/curia/curia.tmux` with `<bin>` substituted:

```
set -g status-interval 2
set -ag status-right ' #(<bin> status)'
set -g window-status-format '#{?#{==:#{@curia_marker},red},#[fg=red]● #[default],}#I:#W'
set -g window-status-current-format '#{?#{==:#{@curia_marker},red},#[fg=red]● #[default],}#I:#W'
bind a display-popup -E -w 90% -h 80% '<bin> ui'
bind N run-shell '<bin> next'
set-hook -g pane-focus-in 'run-shell -b "<bin> seen #{pane_id}"'
```

`~/.tmux.conf` gets one `source-file` line between `# >>> curia >>>` and `# <<< curia <<<`, added once and removed whole by `RemoveTmux`.

`curia install` runs both and prints the next steps: restart running Claude Code sessions and run `tmux source-file ~/.tmux.conf`. `curia uninstall` reverses both.

- [ ] **Step 1: Write the failing tests**

```go
func TestHooksMergePreservesUserHooks(t *testing.T) // user's own Stop hook and unrelated keys survive, ours added beside it
func TestHooksIdempotent(t *testing.T)              // Hooks twice -> exactly one entry of ours per event
func TestHooksBackupWritten(t *testing.T)           // .bak.<unix> equals the original bytes
func TestInvalidJSONUntouched(t *testing.T)         // "{" -> error; file bytes unchanged; no backup written
func TestMissingSettingsCreated(t *testing.T)
func TestUnhooksRemovesOnlyOurs(t *testing.T)       // user's Stop hook remains; empty event arrays removed
func TestTmuxFenceAddedOnceAndRemoved(t *testing.T) // WriteTmux twice -> one fence; RemoveTmux -> conf equals original
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/install/ -v`. Expected: FAIL.
- [ ] **Step 3: Implement** with `encoding/json` into `map[string]any` (so unknown keys round-trip) and the fence as plain text. `bin` is `os.Executable()` resolved through symlinks.
- [ ] **Step 4: Run and confirm they pass.** Same command. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -am "feat: install and uninstall"`

### Task 11: End-to-end replay of five parallel sessions

**Files:**

- Create: `testdata/replay/five_sessions.jsonl`, `testdata/replay/transcripts/*.jsonl`, `e2e/replay_test.go`, `e2e/testdata/ls.golden.json`

**Interfaces:**

- Consumes: the built `curia` binary: `hook`, `ls --json`, `status` (Tasks 5 and 7).
- Produces: the Plan 1 acceptance test.

Each replay line is `{"pane": "%1", "event": "SessionStart", "payload": {...}}`; transcript paths in payloads point into `testdata/replay/transcripts/`. The script:

1. S1 in `%1`: starts, then `PreToolUse` and `PostToolUse` on an Edit. Expected: working.
2. S2 in `%2`: starts, then `Notification` "Claude needs your permission". Expected: waiting.
3. S3 in `%3`: starts, then `Stop` with a transcript holding 142000 context tokens. Expected: idle, unseen, `context_used` 142000.
4. S4 in `%4`: starts. Then S5 starts in `%4` and runs a `PreToolUse` on the `Task` tool. Expected: S4 ended and absent from `ls`; S5 working with 1 subagent.

S1 to S3 run concurrently from three goroutines; S4 and S5 run in order.

- [ ] **Step 1: Write the failing test**

```go
func TestReplayFiveSessions(t *testing.T)
// go build -o $TMP/curia ./cmd/curia; CURIA_DB=$TMP/c.db; HOME=$TMP; no tmux server
// run every replay line as `curia hook <event>` with TMUX_PANE and the payload on stdin
// every run exits 0; p95 wall time per run < 50ms (process start included; the 20ms budget is checked in-process by Task 5)
// `curia ls --json` equals e2e/testdata/ls.golden.json: order S2, S3, S1, S5
// `curia status` prints "W1"
```

- [ ] **Step 2: Run and confirm it fails.** Run: `go test ./e2e/ -v`. Expected: FAIL until the fixtures and golden file exist.
- [ ] **Step 3: Write the fixtures and the golden file** from the script above, by hand, never generated from the program's output.
- [ ] **Step 4: Run the whole suite.** Run: `go test -race ./...`. Expected: PASS.
- [ ] **Step 5: Try it live.** Run `curia install`, start three Claude Code sessions in three tmux windows, and check: the badge counts change within 2 seconds, `prefix + N` jumps to the waiting session, `prefix + a` opens the popup with correct rows.
- [ ] **Step 6: Commit.** `git commit -am "test: end-to-end replay of five parallel sessions"`
