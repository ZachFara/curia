# Curia Implementation Plan 5: Cost, Forecasting and Integrations

Nov 13, 2026 · @Zachariah Farahany

## TLDR

Plan 5 is the last plan of V1. It gives Curia real numbers and its outside connections. `curia ingest` takes one JSON document per turn from cost-lens, or from Curia's own Claude Code status-line command when cost-lens does not push, and writes each session's cost and prompt-cache state plus a series of account usage samples (F15). From those samples the badge warns when the limit will land before the reset, `curia usage` and the dashboard show who is burning it, and Curia suggests pausing the lowest-priority task and starts new agents on a cheaper model (F16). The cache values that Plan 2's queue order has been waiting for become real, with a cache column on the dashboard and the stakes line in the phone push (F17). Decisions export to Obsidian, Linear issues import as tasks, Codex sessions are tracked and spawned, and a global decision can be printed as an Agent Commons entry (F13). It ships in 22 test-first tasks on the integration branch `plan-5`, after Plans 3 and 4 have merged, with a final docs task that closes V1. It is done when the built binary ingests a usage series, shows the forecast in the badge, starts a task on the cheaper model, pushes a ticket with its stakes line, writes an Obsidian note, imports issues from a stand-in Linear server and records a Codex session.

## Contents

- Dependencies on Plans 3 and 4 to re-verify
1. Plan sequence
2. Global constraints
3. Review focus
4. Decided here: schema, ingest, status line, forecast, views, cost-lens, Obsidian, Agent Commons, Linear, Codex, config, inputs
5. File structure
6. Setup (orchestrator, before the first task)
7. Tasks, order and models
8. Tasks 1, 4, 5, 9 and 11 (need nothing)
9. Tasks 2, 3, 6, 7, 8, 10 and 12
10. Tasks 13 to 21
11. Task 22 (acceptance) and Task 23 (V1 docs, orchestrator)
12. Decisions made in this plan
13. Live probes
14. Deferred items: picked up, and cut from V1
15. Questions only the owner can answer
16. Out of scope
17. V1 completion
- Appendix A: the interface and file check

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. An implementer reads `AGENTS.md`, section 2, its own task, and the parts of section 4 named on that task's **Brief includes** line, pasted into the brief verbatim. There is no per-task reviewer and no pre-flight review (owner's ruling): the only review is the final PR, so a task's named tests are its gate and every one of them must exist and pass.

**Goal:** I see what each agent costs and when I will run out, Curia acts on that forecast without ever acting alone, and my decisions and tasks connect to the tools I already use.

**Architecture:** cost and usage arrive only through `curia ingest`, a command that reads one bounded JSON document on stdin, never fails its caller and writes in one transaction: three columns of the session row and two append-only sample tables. Curia computes no price: cost-lens or Claude Code supplies every dollar figure. Forecasting is a pure package, `internal/usage`, over those samples; the badge, `curia usage`, the dashboard and `curia spawn` read it. Nothing is paused or changed by a forecast on its own. Integrations are one-way and opt-in: an Obsidian note is a file written inside a configured folder through `os.Root`; Linear is one read-only GraphQL query; an Agent Commons entry is printed, not sent; Codex reuses the hook adapter with an agent flag. No hook does new work.

**Tech stack:** unchanged (Go 1.26, `modernc.org/sqlite` v1.62.0, cobra, Bubble Tea v1, lipgloss, BurntSushi/toml). No new module: Linear uses `net/http` and `encoding/json`, and the Codex config is read with the TOML library already pinned.

**Spec:** `docs/DESIGN.md`. This plan implements F13, F15, F16, the dashboard column and push line of F17 that Plan 2 deferred, the cost column of F3 and the cost source of F2 that Plan 1 deferred, the cost and cache state of a team's group row (F14), the Linear source of F5, `--agent codex` of F6, the branch cost of a review (F10), and the rules of section 7 for every new input.

**Code as built:** `main` at 33db0e4 (Plans 1 and 2, release packaging, the installer fix of Rulings 94 to 96). Plan 3 is being built on `plan-3`; Plan 4 is planned (PR #53). Every interface this plan takes from Plans 1 and 2 was checked against that code. Every interface it takes from Plans 3 and 4 is quoted from their plan documents in the next section and must be checked again against the merged code.

## Dependencies on Plans 3 and 4 to re-verify

Plan 5 starts only after `plan-3` and `plan-4` have merged into `main`. Before section 6's setup PR the orchestrator checks each line below against the merged code. A difference in a name or signature is corrected in the tasks named, by amending this document; a difference in behaviour is a Ruling.

| # | Item, as its plan states it | Used by tasks |
| --- | --- | --- |
| E1 | Migrations `0004_tasks.sql` (Plan 3) and `0005_reviews.sql` (Plan 4) are the newest; this plan's is `0006_cost.sql`. If a fix wave added another, this plan's takes the next free number and every "6 migrations" below becomes that count. `tasks.source` already allows `'linear'` (0004). `sessions.cost_usd` (0001), `sessions.cache_expires_at` and `sessions.cache_rebuild_usd` (0002) exist and no code writes them. | 6, 7 |
| E2 | `store.upsert` names its columns and never writes `cost_usd`, `cache_expires_at`, `cache_rebuild_usd` or the team columns, so they survive every session write (Plan 3 section 4.1; Plan 2's `TestCacheColumnsSurviveSessionUpsert`). `selectSessions` and `scanSession` are one statement and one scanner in `internal/store/store.go`. | 6 |
| E3 | `model.Task` (Plan 3 Task 1): `N int64`, `ID, Title, Body, Repo, BaseBranch string`, `Status model.TaskStatus`, `Source model.TaskSource`, `SourceRef string`, `Tags []string`, `Branch, Worktree, AgentModel, Pane, ResumeSession string`, `Team bool`, `CreatedAt, UpdatedAt, LaunchedAt, PausedAt, CleanedAt time.Time`; `model.SourceLocal`, `model.SourceBacklogMD`; `model.TaskRunning = "running"`. `model.Session` has `TaskID, Worktree, TeamID, TeamRole, TeamMember string` (Plan 3) and `Socket string`, `ReviewPending bool` (Plan 4). | 1, 7, 15, 16, 17, 21 |
| E4 | `func (s *Store) GetTask(ctx context.Context, id string) (model.Task, bool, error)`; `func (s *Store) ListTasks(ctx context.Context, f TaskFilter) ([]model.Task, error)`; `func (s *Store) UpdateTask(ctx context.Context, id string, fn func(model.Task) (model.Task, error)) (model.Task, error)` ("writes every column but `n`, `id` and `created_at`"); `func (s *Store) CreateTask(ctx context.Context, t model.Task) (model.Task, error)`; `func (s *Store) ImportTask(ctx context.Context, t model.Task) (stored model.Task, created bool, err error)` ("when a row with the same `(repo, source, source_ref)` exists, updates its title, body and tags only"). The task columns are listed in one place in `internal/store/tasks.go`. | 7, 15, 16, 17 |
| E5 | `internal/task` (Plan 3 Task 6): `task.Normalize(t model.Task) model.Task`, `task.Validate(t model.Task) []Problem`, `type Problem struct{ Field, Problem string }`, `task.ValidID`, `task.ValidModel(m string) bool`, `task.MaxTitle = 120`, `task.MaxBody = 48 << 10`; `type Card struct { Task model.Task; Status model.TaskStatus; Sessions []model.Session; Pane string }`; `task.Brief(t model.Task, team bool) string`. | 15, 16, 17, 21 |
| E6 | `cmd/curia/task.go` (Plan 3 Task 16): `curia task add <title> [--repo <dir>] [--body <file>\|-] [--base <branch>] [--tag <t>]…` prints `T-42 added (backlog)`; `curia task edit <id> […]`; `curia task ls [--status <s>] [--repo <dir>] [--json]` with table columns `ID`, `STATUS`, `REPO`, `TITLE`, `AGENT`; the golden file `cmd/curia/testdata/tasks.golden.json`; `taskCards(ctx context.Context, st *store.Store, now time.Time) ([]task.Card, error)`. `cmd/curia/task_import.go` holds `curia task import --backlog-md [<dir>]`, which prints `imported 3, updated 2, skipped 4`, and resolves `--repo` to a main worktree with `gitInfo`. | 15, 16, 21 |
| E7 | `cmd/curia/spawn.go` and `launch.go` (Plan 3 Task 19): `type spawnOptions struct { Model string; Team bool; Resume string; Wait time.Duration }`; `func spawnTask(ctx context.Context, st *store.Store, tm tmux.Client, cfg config.Config, id string, o spawnOptions) (spawnResult, error)`; `func spawnLine(r spawnResult) string`; `var launchExec = syscall.Exec`; `var launchLookPath = exec.LookPath`; `curia spawn <id> [--agent claude] [--model <m>] [--team]`, where "`--agent` accepts only `claude`"; `curia launch <id> [--model <m>] [--team] [--resume <uuid>]` replaces itself with `claude [--model <m>] [--teammate-mode tmux] <brief>` or `… --resume <uuid>`. | 17 |
| E8 | `cmd/curia/pause.go` (Plan 3 Task 24): `curia pause <id> [--force] [--no-verify]`, user-only, "prints one line per step it completed". Plan 3 names no function behind the command. Task 21 calls `pauseTask`; if the merged file has only a cobra `RunE`, Task 21 moves its body into `pauseTask` (signature in Task 21) and edits `pause.go` for that alone. | 21 |
| E9 | `fleet.Counts` has `Decisions, Undelivered, Waiting, Reviews int` and `AutoOff bool`; `fleet.Badge` "appends `auto-off` as a last part when it is set" (Plan 3 Task 29); `fleet.Count(s []model.Session, openBlocking, undelivered int) Counts`. `type Unit struct { Lead model.Session; Teammates []model.Session; Worst model.Session }` and `func Units(sessions []model.Session) []Unit` in `internal/fleet/units.go` (Plan 3 Task 9). | 12, 14, 20 |
| E10 | The dashboard (Plan 3 Task 30, Plan 4 Task 20): rows come from `fleet.Units`; a team is a group row; columns by width "all ten at 120 or more; without `MODEL` and `SUB` from 90; also without `LAST UPDATE` from 60; below 60 only state, window, `TASK` as the id alone, `DEC` and `AGE`"; `ui.Source` has `Sessions`, `OpenDecisions`, `Tasks(ctx context.Context) (map[string]model.Task, error)`; keys in use on the session table: `j` `k` arrows Enter `/` `x` `d` `r` `t` `n` Space `q` Esc. `b` and `P` are free. `ui.Model.WithDecide`, `WithBoard`, the `childMsg` wrapper, and the confirm prompt of `x` (any key but `y` cancels; `y` counts 250 ms after the prompt opened). | 20, 21 |
| E11 | `ui.ReviewDetail` (Plan 4 Task 8) has `Sessions, Teammates int`, `Duration time.Duration`, `CostUSD float64`, `CostKnown bool`; the view's line 7 reads `Sessions: 3 (lead and 2 teammates) · duration 1h12m · cost $4.12`, or `cost not recorded`. `store.TaskRun` "reads `sessions.cost_usd` (a `SUM` over the non-NULL values, `CostKnown` when any is non-NULL)". `curia review <id> --json` prints `cost_usd` or null. The review source's `Detail(ctx context.Context, reviewID string) (ReviewDetail, error)` is in `cmd/curia/review.go` and is called every 5 seconds for the shown review. | 19 |
| E12 | `internal/notify/ntfy.go` (Plan 4 Task 7): `func DecisionPush(d model.Decision, topic, answerURL, token string) Notification`; the message is the question, one line per option, `Recommended: <key>`, then the `Irreversible: …` or `More options: …` line, through `sanitize.Text(…, 1200)`. `cmd/curia/push.go` (Plan 4 Task 15): `pushFlush(ctx context.Context, st *store.Store, cfg config.Config, pub notify.Publisher, now func() time.Time) (int, error)` reads the push's subject again before it publishes. | 11 |
| E13 | `cmd/curia/decide.go` (Plan 2 Task 14, as in `main`): the flag forms and the popup both answer through `deliver.Answer`; `refuseInsideAgent`; `decideVia()`. | 18 |
| E14 | `cmd/curia/install.go` as in `main` plus Plans 3 and 4: `runInstall` runs its steps in order and prints one line each through `installSay`; `executablePath()` returns the path install records, which since Ruling 104 is the stable path (the Homebrew `bin` link, not the versioned Cellar path); `claudeRunner install.Runner`; `install.ClaudeDir(home, configDir string) string`; `loadSettings`, `saveSettings` (backup first, atomic rename, symlinks resolved), `shellQuote` in `internal/install/settings.go`. `install.toolRules` holds six names. | 9, 10 |
| E15 | `hook.Env` has `Pane string`, `Now func() time.Time`, `TaskID string`, `Git func(cwd string) (repo, branch, worktree string)`, `PaneInfo func(pane string) (tmuxSession, window, socket string)`; `runStop` claims answers and messages in one transaction; `state.Apply`'s start sets `s.Agent = "claude"` (`internal/state/state.go`, as in `main`). `cmd/curia/hook.go` registers `hook <event>` with `DisableFlagParsing: true` and `Args: cobra.ArbitraryArgs`. | 8 |
| E16 | `testdata/tasks/claude-stub.sh` (Plan 3 Task 31: the stand-in `claude`, which records its argv and draws the empty prompt box) and the e2e helpers `build`, `newHome`, `normalise`, `startTmux`, `startClient`, `gitInit`, `readGolden`, `compareLines`; Plan 4's stand-in ntfy listener in `e2e/reviews_test.go`. | 22 |
| E17 | Plan 3's rulings: 97 (stored values and identifiers use US spelling: `canceled`); 98 (a team's directory is `session-<8 hex>` and is deleted when the lead exits); 99 (the router call uses `--safe-mode` and no `--json-schema`); 100 (a long text handed over by the Stop hook may be refused by the model); 101 (idle footer variants); 103 (tmux binds `m` and `M` by default; `a`, `A`, `N`, `u` are free); 104 (install records the stable binary path). Plan 5 adds no tmux key, option or format, and types nothing new into a pane. | 9, 10, 20 |
| E18 | `cmd/curia/main_test.go`'s `guardUnset` (as on `plan-3`: `TMUX`, `TMUX_PANE`, `CLAUDECODE`, `CLAUDE_CODE_SESSION_ID`, `CLAUDE_PROJECT_DIR`, `CLAUDE_CONFIG_DIR`, `CURIA_SESSION_ID`, `SSH_CONNECTION`, plus what Plans 3 and 4 add: `CURIA_TASK_ID`, `CURIA_NTFY_TOKEN`). | setup |

## 1. Plan sequence

Row 5 of Plan 1's table: "cost-lens pushes into `curia ingest`; the badge forecasts time to limit; decisions export to Obsidian". No plan follows. What V1 leaves undone is in sections 14, 16 and 17.

## 2. Global constraints

Every task's requirements include these, and every constraint of Plans 1 to 4 still holds unless amended here.

- **Hook budget and output.** Unchanged: under 20 ms per event; nothing on stdout or stderr and exit 0 on every path, except the one `{"decision":"block","reason":"…"}` line of a `Stop` that claimed something. Plan 5 adds to the hook path exactly one thing: `curia hook <Event> --agent codex` sets one string on the event (Task 8), and for such a session the hook skips the transcript read and the Stop claim, so it does less, not more. No process, statement, file read or network call is added. Ingest, forecasting, exports and imports never run in a hook. Measured by the existing latency tests, unchanged.
- **`curia ingest` and `curia statusline` never fail their caller.** Exit 0 on every path, panics included. `ingest` writes nothing to stdout or stderr. `statusline` writes one line to stdout (it is Claude Code's status line) and nothing to stderr. A bad document is logged and dropped. Budget: p95 under 20 ms in-process and under 50 ms as a process, measured as the hook is (Task 13, Task 22).
- **Only I decide, approve, merge or pause (spec section 7).** No MCP tool is added: the surface stays at Plan 4's six. A forecast never pauses, kills or changes anything by itself: the pause suggestion needs my key and my `y`, and the cheaper model is a default I override with `--model`. `curia ingest` accepts numbers from any local process, an agent included, so nothing automatic may depend on them.
- **Everything from outside is data.** An ingest document, Claude Code's status-line JSON, the cost-lens analyzer's output, a Linear response, a Codex hook payload and config file, an existing file in the Obsidian folder, and decision text on its way into a note or an entry are untrusted. Each is bounded and cleaned by `internal/sanitize` (or parsed into numbers and enums) when it enters and again wherever it is printed or written; section 4.13 lists every new input. None is executed, interpolated into a script, passed to a shell or built into SQL.
- **Nothing leaves the machine unless the spec says so and I configured it.** Plan 5 opens exactly one kind of connection: `curia task import --linear`, a command I run, to the configured Linear URL. Ingest, forecasting, the Obsidian export, the Agent Commons entry and the cost-lens analyzer (run without `--live-prices`) use no network. No test touches the real network: Linear is a local `httptest` server fed recorded or built responses.
- **Secrets.** The Linear API key is read from the environment variable `CURIA_LINEAR_API_KEY` (a Keychain item `env:CURIA_LINEAR_API_KEY` on the owner's machine) and never from a file Curia writes or reads. It is sent only in the `Authorization` header of a request to the configured Linear URL, never logged, never printed, never stored, and no error text contains it. Curia never reads `~/.codex/auth.json` or any credential file.
- **Typing into panes.** Unchanged, and Plan 5 types nothing: Rulings 81 to 93 stand as `AGENTS.md` lists them. A Codex pane is never typed into, because `deliver.readPrompt` recognizes only Claude Code's prompt and anything else types nothing.
- **Installer rulings.** Ruling 94 (never replace `window-status-format`), 95 (never rebind a key the user bound), 96 (change tmux options only as far as needed): Plan 5 changes nothing in the tmux snippet, binds no key and sets no option; the forecast is text inside the existing `#(curia status)`. The same spirit governs Claude Code's and Codex's settings: Curia never replaces a `statusLine` or a hook entry that is not its own, and reports what it left alone. Ruling 104: every command Curia registers (the status line, the Codex hooks and MCP entry) records `executablePath()`, the stable path.
- **One commit per task.** Implementers share one `make check` lock across worktrees. Before the single gated commit an implementer runs, on its own packages only, `gofumpt -l`, `.bin/golangci-lint run ./<its packages>/...`, `go vet` and the focused `go test -race` of its steps, and fixes what they show; then it commits once (the pre-commit hook runs the whole `make check`). A task is one commit unless the gate itself fails.
- **Spelling.** US spelling in identifiers, strings and stored values (`canceled`, `recognized`, `normalize`): the linter enforces it (Ruling 97).
- **One write transaction per read-modify-write** in `internal/store`: an ingest (the session's three columns, the samples and the pruning together), each export mark.
- **Real dependencies in tests.** SQLite files under `t.TempDir()`; a real tmux server on socket `curia-test-<pid>` started with `-f /dev/null`; real git repositories under `t.TempDir()` with `gitEnv`; `net/http/httptest` servers on loopback for Linear and ntfy; real directories for the Obsidian folder; the built binary in `e2e/`. Fakes only for what cannot run in CI: `tmux.Fake`, the `ui` source interfaces, the seams for `claude`, `codex` and `node`. Claude Code and Codex are replaced by stubs and by recorded payloads under `testdata/`; Linear by recorded or built responses, never a live call. Timing-sensitive code takes an injected clock.
- **One file per command** in `cmd/curia/`, registered from that file's `init()`. Tasks add to package `main` in parallel: every new package-level name starts with its file's prefix (`ingest`, `statusline`, `usage`, `export`, `costlens`, `linear`), in tests too.
- **Shared files** (`go.mod`, `go.sum`, `Makefile`, `.golangci.yml`, `lefthook.yml`, `scripts/`, `.github/`, `.goreleaser.yaml`, `README.md`, `cmd/curia/main.go`, `cmd/curia/main_test.go`, `AGENTS.md`, `PROGRESS.md`, `docs/`) are changed only by the orchestrator, in section 6 and in Task 23.
- **Timestamps** in new columns use the store's fixed-width UTC layout; a zero time is NULL. Lint limits, `gosec` rules and the ban on suppressions are unchanged; `go mod tidy` is still never run.

## 3. Review focus

What the final whole-branch reviewer must check for this plan in particular. Each has a named test in the task that owns the code.

1. **An ingest that hurts its caller or the store.** Empty stdin, 1 MiB of text, a JSON array, `schema_version` 2, a session id holding `'; DROP`, a cost of `-1`, `1e308` or `"4.12"`, a reset time in 2099, a closed store, a panic. Expected: exit 0, nothing printed, the document or the one field dropped and logged by field name only, no partial write. Task 2: `TestParseDocRefusals`, `TestParseDocDropsBadFieldsOnly`. Task 13: `TestIngestNeverFails`, `TestIngestPrintsNothing`.
2. **A forged or stale number steering me.** An agent runs `curia ingest` with 99% used; one sample; samples from before a reset; a clock that jumps; two sessions reporting the same account window. Expected: no warning from fewer than two samples or less than 3 minutes of span; a drop in `used_pct` starts a new series; the two sessions merge into one series; and whatever the numbers say, nothing is paused and no model is forced. Task 3: `TestForecastNeedsSpan`, `TestForecastCutsAtReset`, `TestForecastMergesSessions`. Task 17: `TestSpawnModelFlagBeatsForecast`. Task 21: `TestPauseNeedsKeyThenConfirm`.
3. **Ingest erasing what it does not carry.** The status-line path (no cache fields) runs after a cost-lens push (with them), or a hook upserts the session between two ingests. Expected: a field absent from a document changes nothing; `updated_at` and `seen_at` are untouched, so an ingest never looks like activity. Task 6: `TestIngestAbsentFieldsChangeNothing`, `TestIngestIsNotActivity`, `TestIngestSurvivesConcurrentHook`.
4. **Sample tables growing without bound.** Ten sessions ending turns for a week. Expected: an identical reading within 60 seconds adds no row; each ingest prunes at most 200 old rows; usage samples older than 8 days and cost samples older than 24 hours go. Task 6: `TestIngestSkipsRepeatedReading`, `TestIngestPrunesBounded`.
5. **Decision text escaping an Obsidian note or its folder.** A question holding a fence of five backticks, `![[secret]]`, a `dataviewjs` block, `---` lines, `../../x`, a NUL, 300 runes of emoji; a folder that is a symlink; a file in the folder that looks like ours but is mine. Expected: every agent or user string sits inside a fence it cannot close; the file name has no path separator; nothing is written outside the folder (`os.Root`); a note without Curia's marker is never overwritten; nothing is ever deleted. Task 4: `TestFenceCannotBeClosed`, `TestNoteTitleIsAPlainName`. Task 18: `TestExportStaysInsideFolder`, `TestExportLeavesForeignNote`.
6. **Linear written to, or my key leaking.** A response that redirects, a 401, a GraphQL error holding the key's text, a 100 MiB body, an issue whose title is escape sequences, a description of 1 MiB. Expected: every request is the one constant query (no `mutation` anywhere in the client); no redirect is followed; the key appears in no output, log or error; text is bounded and cleaned before it is stored. Task 5: `TestClientSendsOnlyTheQuery`, `TestClientDoesNotFollowRedirect`, `TestErrorsNeverHoldTheKey`. Task 16: `TestLinearImportCleansText`.
7. **Replacing something of mine in Claude Code's or Codex's settings.** A `statusLine` I already have; one of an unexpected type; a Codex hooks file with my own entries. Expected: left byte for byte, reported with what to do; uninstall removes only Curia's entries; a backup is written before any change. Task 9: `TestStatusLineForeignUntouched`, `TestRemoveStatusLineOnlyOurs`. Task 10: `TestCodexHooksKeepUserEntries`.
8. **The status line breaking Claude Code's screen.** A payload with escape sequences in the model name, no cost, no limits, 1 MiB. Expected: one line of at most 80 cells with no control rune, or an empty line; exit 0. Task 2: `TestStatusTextIsOneCleanLine`. Task 13: `TestStatuslineAlwaysPrintsOneLine`.
9. **A Codex session treated as Claude Code.** A Codex Stop while an answer is queued; a Codex transcript path; an answer for a Codex session. Expected: nothing is printed at its Stop and nothing claimed; its transcript is not parsed; nothing is typed; the answer is collected by `check_decision`. Task 8: `TestCodexStopClaimsNothing`, `TestCodexTranscriptNotRead`.
10. **The cost-lens analyzer hanging or lying.** It runs for minutes, prints 100 MiB, prints HTML, names a branch with escape sequences, is not installed. Expected: one run at a time, a 90 second limit, a 4 MiB cap, a number or nothing; the review never waits for it. Task 2: `TestBranchCostRefusals`. Task 19: `TestCostlensRunIsBounded`, `TestReviewDetailNeverWaits`.
11. **Old and new binaries on one database.** A hook or `curia mcp` started before the upgrade keeps running. Expected: an older upsert leaves the cost and cache columns alone; an older `UpdateTask` (which writes "every column but `n`, `id` and `created_at`" that it knows) leaves `priority` and `agent` alone. Task 6: `TestMigrationOnPlan4Database`, `TestOldUpsertKeepsCostColumns`. Task 7: `TestOldTaskUpdateKeepsPriority`.

## 4. Decided here

Implementers do not choose any of this. Reasons and costs are in section 12.

### 4.1 Migration `internal/store/migrations/0006_cost.sql`

Exact content. Verified at the desk with the system `sqlite3` 3.51.0 on stand-in `sessions`, `tasks` and `decisions` tables: it runs as one script in one transaction; `window` is accepted as a column name; `priority = 4` and `window = 'one_hour'` fail their `CHECK`; an existing task row reads `priority` 2 and `agent` `claude` afterwards. Task 6 proves it on the pinned `modernc.org/sqlite` after 0001 to 0005.

```sql
-- Plan 5: usage and cost samples, task priority and agent, the export mark.
-- Differences from spec F16 are listed in the plan (section 4.1).

CREATE TABLE usage_samples (
  n          INTEGER PRIMARY KEY,
  at         TEXT NOT NULL,                  -- when the reading was taken
  window     TEXT NOT NULL CHECK (window IN ('five_hour','seven_day')),
  used_pct   REAL NOT NULL CHECK (used_pct >= 0 AND used_pct <= 100),
  resets_at  TEXT,                           -- NULL when the reading had none
  session_id TEXT NOT NULL DEFAULT ''        -- whose ingest carried it; the windows are account-wide
);
CREATE INDEX usage_samples_window ON usage_samples(window, at);

CREATE TABLE cost_samples (
  n          INTEGER PRIMARY KEY,
  at         TEXT NOT NULL,
  session_id TEXT NOT NULL,
  cost_usd   REAL NOT NULL CHECK (cost_usd >= 0)   -- the session's running total
);
CREATE INDEX cost_samples_session ON cost_samples(session_id, at);

ALTER TABLE tasks ADD COLUMN priority INTEGER NOT NULL DEFAULT 2 CHECK (priority IN (1,2,3));
ALTER TABLE tasks ADD COLUMN agent TEXT NOT NULL DEFAULT 'claude' CHECK (agent IN ('claude','codex'));
ALTER TABLE decisions ADD COLUMN exported_at TEXT;  -- when its Obsidian note was last written
```

Differences from the spec. `usage_samples` has `n` and `session_id` beside the spec's four columns. `cost_samples` is new: the API math and the attribution of F16 need each session's cost over time, and `sessions.cost_usd` holds only the latest. `tasks.priority` is an integer 1 to 3 shown as `P1` to `P3`. `tasks.agent` and `decisions.exported_at` are new. No column is added to `sessions`: the three that ingest writes exist since migrations 0001 and 0002.

`selectSessions` and `scanSession` read `cost_usd`, `cache_expires_at` and `cache_rebuild_usd` from now on; `upsert` still never writes them.

### 4.2 The ingest contract

`curia ingest` reads one JSON document of at most 64 KiB (`usage.MaxDoc`) from stdin. The schema lives in `docs/ingest-schema.json` (setup); the parser is `usage.ParseDoc`.

| Field | Rule | Stored |
| --- | --- | --- |
| `schema_version` | required, the number 1 | no |
| `session_id` | required, `^[A-Za-z0-9_-]{1,64}$` | the key |
| `at` | optional, RFC 3339: when the reading was taken; accepted from `now − 24h` to `now + 1m`; default `now` | the samples' `at` |
| `cost_usd` | optional number, 0 to 100000 | `sessions.cost_usd`, a cost sample |
| `cache_expires_at` | optional, RFC 3339, from `now − 7d` to `now + 2h` | `sessions.cache_expires_at` |
| `cache_rebuild_usd` | optional number, 0 to 1000 | `sessions.cache_rebuild_usd` |
| `rate_limits.five_hour`, `rate_limits.seven_day` | optional objects: `used_percentage` a number 0 to 100 (required in the object); `resets_at` RFC 3339 or a number of Unix seconds, from `now − 1h` to `now + 8d`, else the reading is kept without a reset time | one usage sample each |
| `model`, `context_used`, `context_max` | read and ignored: hooks and the transcript own them (F2) | no |
| anything else | ignored | no |

Rules.

- A document that is not one JSON object, is too large, has another `schema_version` or a bad `session_id` is refused whole (`usage.ErrDoc`). A bad optional field is dropped alone and its name goes into `Doc.Dropped`; the rest is stored. The log line (WARN) names the reason and the dropped field names, never a value.
- An absent field changes nothing. `null` counts as absent. Ingest never clears a stored value.
- `store.Ingest`, one `BEGIN IMMEDIATE` transaction: (1) `UPDATE sessions SET cost_usd = COALESCE(?, cost_usd), cache_expires_at = COALESCE(?, cache_expires_at), cache_rebuild_usd = COALESCE(?, cache_rebuild_usd) WHERE id = ?`, which touches neither `updated_at` nor `seen_at`; no row is created for an unknown session; (2) when that session exists and the document has a cost: a cost sample, unless the session's newest sample has the same cost and is less than 60 seconds older; (3) per window: a usage sample, unless the window's newest sample has the same `used_pct` and `resets_at` and is less than 60 seconds older; usage samples are stored for an unknown session too, because the windows belong to the account; (4) at most 200 rows deleted, oldest first: usage samples older than 8 days, cost samples older than 24 hours.
- The command: exit 0 always, no output, a 2 second limit on the whole run, `SIGPIPE` ignored as the hook does. `curia ingest --statusline` reads Claude Code's status-line JSON instead (section 4.3) and stores it the same way.
- `curia ingest` is allowed inside an agent session: cost-lens runs it from inside Claude Code. That is why nothing automatic depends on its numbers (section 2).

### 4.3 The status line

Claude Code runs a configured `statusLine` command with a JSON document on stdin and shows its first line of stdout. The fields below are assumed until probe S1 records a real payload as `internal/usage/testdata/statusline-captured.json`; Task 2's field names are amended to it.

| Status-line field (assumed) | Becomes |
| --- | --- |
| `session_id` | `session_id` |
| `cost.total_cost_usd` | `cost_usd` |
| `rate_limits.five_hour.used_percentage`, `.resets_at` (RFC 3339 or Unix seconds); the same under `seven_day` | the two readings |
| `model.display_name`, else `model.id` | the printed model name only |
| `context_window.used_percentage` | the printed context percentage only |

`usage.FromStatusLine` maps it to a `usage.Doc` under the bounds of section 4.2; there is no `schema_version` and no cache field, so cache state stays as it is (blank when nothing else supplies it, as the spec says).

`curia statusline` stores the document as `ingest` does, then prints `usage.StatusText`: `<model> · ctx 71% · $4.12 · 5h 62%`, each part left out when its value is absent, the model name through `sanitize.Line(…, 24)`, the whole at most 80 cells. On any failure it prints an empty line. Exit 0 always.

`curia install` registers it (Task 9):

- `install.StatusLine(settingsPath, bin, now)` reads `settings.json` as `Hooks` does. No `statusLine` key: it writes `"statusLine": {"type": "command", "command": "<shell-quoted bin> statusline"}` (backup first) and returns `StatusLineAdded`. A `statusLine` whose `command` is exactly that string: `StatusLineOurs`, nothing written. Anything else, a value of another type included: `StatusLineForeign`, nothing written.
- `curia install` runs it by default and prints one line: `status line: registered curia statusline (cost and usage for the badge; skip with --no-statusline)`, `status line: already registered`, or `status line: you have your own, left as it is; to feed Curia from it, pipe its input through: tee >(curia ingest --statusline)`. `--no-statusline` skips the step.
- `curia uninstall` calls `install.RemoveStatusLine`, which deletes the key only when its command is Curia's.

### 4.4 Forecast rules

All pure, in `internal/usage/forecast.go`, with `now` passed in.

- **Series.** For one window: the samples with `at` in `[now − 20m, now]` (`usage.BurnWindow`), ordered by `at` then `n`, whatever session carried them; then the longest suffix in which `used_pct` never decreases (a decrease means the window reset in between).
- **Burn.** Needs at least two samples in the series whose first and last are at least 3 minutes apart (`usage.MinBurnSpan`). Rate = (last `used_pct` − first `used_pct`) / (last `at` − first `at`). No forecast when the rate is zero or the rule is not met.
- **Time to limit.** `LimitAt` = last `at` + (100 − last `used_pct`) / rate. `Warn` is true when the last sample has a reset time and `LimitAt` is before it. A last `used_pct` of 100 gives `LimitAt` = last `at` and `Warn` true while the reset is still ahead.
- **Badge text** (`usage.BadgeText`): `5h 62% → limit ~48m (resets 1h50m)`; the label is `5h` or `7d`; the percentage is rounded; durations by `usage.Short` (`48m`, `1h50m`, `3d4h`, never below `1m`); `5h 100% → at limit (resets 1h50m)` when `LimitAt` is not after `now`. When both windows warn, `usage.Warning` returns the one whose `LimitAt` is sooner. The badge shows it after the counts and before `auto-off`; a badge with a warning and no counts is the warning alone.
- **API math** (`usage.APIProjection`). Applies when the last 20 minutes hold no usage sample and at least one cost sample. Per live session: its cost at the last sample in the window minus its cost at the last sample at or before the window's start (or the first sample in the window when there is none before); a negative difference counts as zero. The projection is the sum × 6 (2 hours over 20 minutes). Shown by `curia usage` and on the dashboard's status line as `next 2h ≈ $14 at the current rate`; not in the badge.
- **Attribution** (`usage.Burns`): the same per-session difference × 3, in dollars per hour, for every session with a cost sample in the window. Dollars stand in for the spec's "token deltas": the ingest contract carries no token counter.
- **Pause suggestion** (`usage.SuggestPause`). Only while `usage.Warning` reports a warning. Among tasks whose derived status is `running`: the highest priority number (P3 before P2 before P1); ties by the higher burn; then by the higher `n`. None when no task is running.
- **Cheaper model.** In `spawnTask`, when a warning is in force, the agent is Claude Code, `spawnOptions.Model` is empty, `spawnOptions.Resume` is empty and the task has no `agent_model`: the model is `usage.cheaper_model` from the config (default `sonnet`; empty turns this off), and `spawn` prints `limit forecast (<badge text>): starting T-42 on sonnet; pass --model to choose`. The cost-lens model advisor is not used: it exists only inside the cost-lens mod and has no command to call.
- **Cells.** `usage.CostCell`: `$4.12`, `$0.07`, `$123` from 100 dollars up, blank when unknown. `usage.CacheCell`, for a session that is `idle`, `waiting` or `needs_decision` and blank for any other state: `12m` while warm (at least `1m`), `cold $1.68` when the expiry has passed and a rebuild cost is known, `cold` when it is not, blank when the cache state is unknown.

### 4.5 Views

- **Badge.** `fleet.Counts` gains `Forecast string`; `fleet.Badge` appends it after the counts and before `auto-off`. `curia status` fills it from `store.UsageSince(now − 20m)`: one indexed read more; it still prints nothing on any error and calls no tmux. The status bar's length is not touched (Ruling 96): a long badge is cut by tmux at the user's `status-right-length`.
- **`curia usage [--json]`.** Read-only, allowed anywhere. Prints, in order: one line per window with a sample in the last 20 minutes (`5h 62% · resets 1h50m · burning 51%/h → limit ~48m`, or `… · steady`); in API mode the projection line; `Burn by session` with at most ten rows (`SESSION:WINDOW`, `TASK`, `$/h`, `COST`), highest burn first; and, while a warning is in force and a task qualifies, `suggestion: pause T-7 (P3) api-server: curia pause T-7`. With nothing ingested in the last 24 hours: `no usage has been ingested; see curia install (status line) or cost-lens`. `--json` prints one object: `windows` (array of `window`, `used_pct`, `resets_at`, `limit_at` or null, `warn`), `api_projection_usd` or null, `burn` (array of `session_id`, `task_id`, `usd_per_hour`, `cost_usd`), `suggest_pause` (a task id or null).
- **Dashboard (popup).** Two columns after `DEC`, shown at 100 columns or more: `COST` (`usage.CostCell`, right-aligned) and `CACHE` (`usage.CacheCell`; yellow under 5 minutes, dim when cold). A team's group row shows the sum of its members' known costs and its cache state: the soonest warm expiry among members that are idle, waiting or needs_decision, else `cold` when one of them is cold, else blank. `b` toggles "sort by burn": units ordered by their members' summed burn, highest first, ties in the normal order; the header shows `sorted by burn (b)`; `b` again restores the rank. The status line under the table shows the forecast warning or the API projection when there is one. The sidebar is unchanged.
- **Pause suggestion (popup, not in the sidebar, not inside an agent session).** While `Source.Budget` reports a suggestion, a notice line reads `limit ~48m before reset: P pauses T-7 (P3) api-server`. `P` opens `pause T-7 "Fix the auth refresh"? it commits work in progress and closes its pane  y/N`; `y`, at least 250 ms after the prompt opened and never from a paste, calls `Source.Pause`; any other key cancels. The result line of the pause is shown as a notice. `P` with no suggestion says `nothing to pause: no limit forecast`.
- **`curia task`.** `add` and `edit` take `--priority P1|P2|P3` (also `1`, `2`, `3`; default P2). `ls` gains a `PRI` column after `ID`; `--json` gains `"priority": 2` and `"agent": "claude"`.
- **Review.** Line 7 of Plan 4's section 4.5 gains ` · branch $5.03 (cost-lens)` when the analyzer has answered for the task's branch; nothing is added while it runs or when it fails. `curia review <id> --json` gains `branch_cost_usd` (a number or null).
- **Push.** The message of a blocking ticket's push gains `decision.Stakes` as its last line when the asking session's cache state is known: `answer within 12m to keep cache warm (rebuild ≈ $1.68)` or `cache cold: next turn re-writes ~84k tokens`. It is computed when the push is sent.

### 4.6 The cost-lens analyzer (pull)

Verified at the desk on this machine with `@zachfara/cost-lens` 0.3.0: the `cost-lens` on `PATH` is a symlink to `<package>/terminal/cli.js`; `<package>/analyzer/cli.js` run as `node <analyzer> --project <repository folder>` prints one JSON object whose `currentProject.branches` is an array of `{"branch": "<name>", "spend": {"requests": n, "lastActive": "…", "cost": {"input": x, "output": x, "cacheWrite": x, "cacheRead": x}}}`; it took 12 seconds here and used no network (no `--live-prices`). A redacted copy of that output is the fixture `internal/usage/testdata/summary-captured.json` (setup).

- **Finding it** (`costlensAnalyzer`, Task 19): `costlens.analyzer` from the config when set; else `exec.LookPath("cost-lens")`, `filepath.EvalSymlinks`, and when the result ends in `/terminal/cli.js`, the sibling `analyzer/cli.js` if it is a regular file. `node` by `exec.LookPath`. Either missing: no branch cost, logged once at INFO.
- **Running it:** `[node, <analyzer>, "--project", <task.Repo>]`, no shell, directory `<task.Repo>`, stdin `/dev/null`, the environment as it is, a 90 second limit, `cmd.WaitDelay` set, stdout read up to 4 MiB and then cut. One run at a time per process; a result is kept for 5 minutes per repository.
- **Reading it** (`usage.BranchCost`): the four cost parts of the entry whose `branch` equals the task's branch, summed; `ok` is false when the document is not an object of that shape, the branch is absent, or any part is not a finite number from 0 to 1000000.
- **In the review:** `Detail` never waits. The first `Detail` for a review starts the run in a goroutine; later calls return the kept value. Probe C2 settles whether sessions in a sibling worktree are counted under the main repository's `--project`.

### 4.7 Obsidian export

- **Config.** `[obsidian] dir`: the folder inside the vault where notes go (for example `~/Vault/Decisions`); empty turns the export off. It must exist and be a directory; Curia never creates it.
- **What.** Every decision that is decided (`answered`, `confirmed`, `overridden`; standing rules included), and again after it changes (a rescope or a supersession). Withdrawn and expired tickets are never exported.
- **Name.** `<id> <title>.md`, or `<id>.md` when the title is empty. The title is `export.NoteTitle(question)`: `sanitize.Line(question, 60)`, every rune that is not a letter, a digit, a space or one of `-_(),'` replaced by a space, spaces squeezed, trimmed, trailing dots removed.
- **Finding an existing note.** A regular file directly in the folder named `<id>.md` or starting with `<id> ` and ending in `.md`. If it holds Curia's marker (a front-matter line `curia_id: <id>` within its first 20 lines) it is rewritten in place under the name it has, so a rename of mine after the id survives. If it has no marker it is left alone and reported as `foreign note`. With none, the note is created under its computed name.
- **Content** (`export.Note`), at most 64 KiB:

````
---
curia_id: D-117
status: answered
scope: repo
mode: blocking
repo: "api-server"
task: "T-42"
date: 2026-11-13
tags: ["db", "events"]
---
# D-117 Postgres or Redis for events

## Question
```
<question>
```
## Context
```
<context, cut to 8 KiB>
```
## Options
- A
```
<label>: <tradeoffs>
```
- B (recommended) (chosen)
```
<label>: <tradeoffs>
```
## Answer
```
<answer key and label, or the free text>
```
## Reasoning
```
<my reasoning>
```
````

  `status` is `superseded` when the decision is. `repo` is the base name of the repository path; `repo`, `task` and each tag are JSON-quoted, which YAML reads as a double-quoted string. `date` is the answer's date in UTC. A section with no text is left out. Every string an agent or I wrote sits inside a fence built by `export.Fence`: a run of backticks one longer than the longest run in the text, at least three, with no info string, so no text can close it and no plugin treats it as a query or a template. The heading uses `NoteTitle`, which holds no markup character.
- **Writing** (`cmd/curia/export.go`): the folder is opened with `os.OpenRoot`; the note is written to a temporary name inside the root (mode 0600) and renamed over its target inside the root. Nothing outside the folder can be reached, a symlink in the folder that points outside is refused by `os.Root`, and nothing is ever deleted.
- **When.** `curia export obsidian` exports what is due: decided decisions whose `exported_at` is NULL or earlier than their `answered_at` or `superseded_at` (`store.ExportQueue`, at most 500 a run); `--all` re-exports everything; naming ids exports those. After each successful write `store.MarkExported`. `curia decide` runs the same sweep after an answer (flag forms: before it exits; popup: when it closes), best effort: a failure is logged and one line `obsidian export failed: see the log` is printed by the flag forms only. An answer given from the phone is exported by the next sweep.
- **Output.** `exported 3 notes to ~/Vault/Decisions (1 foreign note left alone: D-9)`; `nothing to export`; with the folder unset: exit 1, `obsidian.dir is not set`.

### 4.8 Agent Commons entry

`curia export commons <id>` prints one JSON object, the arguments of Agent Commons's `propose` tool, and sends nothing. Verified at the desk against the Agent Commons repository on this machine: `propose` takes `symptom`, `environment` (an object), `cause`, `fix` and `domains`, plus `task_id` and `model`, which belong to the proposing agent's own session and are not Curia's to fill.

- Only a decision that is decided, not superseded, and of scope `global`: otherwise exit 1 with `D-117 is not a decided global decision` (`export.ErrNotPublishable`).
- `export.CommonsEntry`: `symptom` = `Decision: ` + the question (500 runes); `environment` = `{"source": "curia", "kind": "decision"}`; `cause` = the context (1500 runes), then `Options considered: A: <label>; B: <label>`; `fix` = `Chosen: <answer key and label, or the free text>. Reasoning: <reasoning>` (2000 runes); `domains` = the tags. Every string through `sanitize.Text`. No repository path, task id, session id or branch is included.
- On stderr, one line: `this is the agent's and your own text: read it before you publish it`.

### 4.9 Linear import

`curia task import --linear [--repo <dir>] [--team <KEY>]`.

- **Key.** `CURIA_LINEAR_API_KEY`, 1 to 200 bytes of `[A-Za-z0-9_.-]`; missing or malformed: exit 1, `set CURIA_LINEAR_API_KEY to a Linear API key (read access is enough)`.
- **Request** (`linear.Client.Issues`). `POST <linear.url>` (default `https://api.linear.app/graphql`) with `Content-Type: application/json`, `Authorization: <key>`, `User-Agent: curia`, and the body `{"query": <linear.Query>, "variables": {"first": 50, "after": <cursor or null>, "filter": <filter>}}`. `linear.Query` is one constant:

```graphql
query($first: Int!, $after: String, $filter: IssueFilter) {
  issues(first: $first, after: $after, filter: $filter, orderBy: updatedAt) {
    nodes { identifier title description priority url state { type } labels { nodes { name } } }
    pageInfo { hasNextPage endCursor }
  }
}
```

  The filter is `{"assignee": {"isMe": {"eq": true}}, "state": {"type": {"in": ["backlog", "unstarted"]}}}`, plus `"team": {"key": {"eq": "<KEY>"}}` when `--team` or `linear.team` gives one (`^[A-Z][A-Z0-9]{0,9}$`). At most 4 pages (200 issues), 20 seconds per request, 4 MiB read per response, no redirect followed. A status other than 200 is an error naming the status only; a body with `errors` is an error holding the first message through `sanitize.Line(…, 200)`. This shape follows Linear's published API and is unverified here: probe L1 (the owner's) records a real response as `internal/linear/testdata/issues-captured.json`; until then the tests use `issues-built.json`.
- **Mapping.** `source` `linear`; `source_ref` the identifier, kept only when it matches `^[A-Z][A-Z0-9]{0,9}-[0-9]{1,7}$` (else the issue is skipped); title through `sanitize.Line(…, 120)`, skipped when empty; body = `Linear <identifier>: <url>` (the url only when it matches `^https://linear\.app/[A-Za-z0-9._/-]{1,200}$`), a blank line, and the description through `sanitize.Block`, cut to `task.MaxBody`; tags = the label names lowercased, every run outside `a-z0-9_-` as one `-`, trimmed of `-`, the first 8 distinct ones that then match the tag rule; priority: Linear 1 or 2 → P1, 0 or 3 → P2, 4 → P3; repository and base branch as `curia task add --repo` resolves them; status `backlog` when new.
- **Storing.** `store.ImportTask`, which for an issue seen before updates title, body and tags only: my priority, status and everything spawn wrote stay. Prints `imported 3, updated 2, skipped 1`.
- **Read-only.** The client has one exported method and one query. Nothing in Curia sends a mutation, and `status` is never mapped: a task's status is derived locally (F5).

### 4.10 Codex

Verified at the desk with `codex-cli` 0.166.0: `codex features list` reports `hooks` as stable and on; `codex [OPTIONS] [PROMPT]` takes `-m, --model <MODEL>`; `codex resume [SESSION_ID] [PROMPT]` resumes by UUID; `codex mcp add <NAME> -- <COMMAND>...` registers a stdio server; the configuration lives in `~/.codex/config.toml` (`$CODEX_HOME` when set); `--dangerously-bypass-hook-trust` exists, so hooks need a recorded trust. Not verified: where hooks are declared, what their payloads hold, what a Stop hook's output does, and whether a hook sees `TMUX_PANE` (probes X1 and X2).

- **Hook.** `curia hook <Event> --agent codex` (the flag is read by hand after the event name, since flag parsing is off for `hook`; any other value is logged and treated as Claude Code). `hook.Env.Agent` carries it; `hook.Run` copies it to `model.Event.Agent`; `state.Apply` gives a new session that agent, `claude` when empty. For an event whose agent is `codex`: the transcript is not read (no model, no context percentage), and a Stop claims no answer and no message and prints nothing. An event name Curia does not know is logged and ignored, as today.
- **Delivery.** An answer or message for a Codex session is collected by `check_decision` and `check_messages`; nothing is typed (the pane check refuses any screen that is not Claude Code's prompt) and nothing is handed over at Stop until probe X1 shows what Codex does with a Stop hook's output.
- **Install.** `curia install --codex` (never by default) registers the MCP server with `codex mcp add curia -- <bin> mcp` through a `Runner` seam and writes Curia's hook entries where probe X1 found Codex reads them (assumed: `<codex dir>/hooks.json`, the same `hooks` shape as Claude Code's `settings.json`, each command `<bin> hook <Event> --agent codex`), backup first, only Curia's entries added. `curia uninstall` removes them and runs `codex mcp remove curia` when the entry is Curia's. Task 10 is not dispatched until X1 and X2 have recorded their fixtures.
- **Spawn.** `curia spawn <id> --agent codex` stores `tasks.agent`; `curia launch` then replaces itself with `codex [--model <m>] <brief>`, or `codex resume <uuid>` when resuming, in an environment holding `CURIA_TASK_ID`. `--team` with a Codex task is refused (`agent teams are Claude Code's`). The cheaper-model default does not apply. No sandbox or approval flag is ever passed.

### 4.11 Config

```toml
[usage]
cheaper_model = "sonnet"     # the model a spawn defaults to under a limit forecast; empty: off

[obsidian]
dir = ""                     # the vault folder for decision notes; empty: no export

[linear]
url  = "https://api.linear.app/graphql"
team = ""                    # a team key such as "ENG"; empty: every team

[costlens]
analyzer = ""                # path of cost-lens's analyzer cli.js or cli.ts; empty: found from cost-lens on PATH
```

`config.Load` rejects, with an error naming the key: `usage.cheaper_model` not matching `^[A-Za-z0-9._\[\]-]{1,64}$` (empty is allowed); `obsidian.dir` or `costlens.analyzer` that, after `~` is expanded, is not absolute, is over 1024 bytes or holds a rune below `0x20`; a `costlens.analyzer` not ending in `.js` or `.ts`; a `linear.url` that is over 200 bytes, does not parse, has no host, has user info, a query or a fragment, or is `http` to a host that is not `localhost` or a loopback address; a `linear.team` not matching `^[A-Z][A-Z0-9]{0,9}$`. The Linear URL comes only from the config file, never from a flag or an argument (Plan 4 found at the desk that `gosec` reports a request URL derived from a command-line argument as G704 and accepts one read from the config file).

### 4.12 Ids and bounds

| Text | Bound | Cleaner |
| --- | --- | --- |
| ingest document, status-line document | 64 KiB | `usage.ParseDoc`, `usage.FromStatusLine`: numbers, times and one id pattern |
| model name on the status line | 24 runes | `sanitize.Line` |
| note title | 60 runes | `export.NoteTitle` |
| note | 64 KiB; context 8 KiB | `export.Note`, `export.Fence` |
| Linear title, description | 120 runes; 48 KiB | `sanitize.Line`, `sanitize.Block` |
| analyzer output | 4 MiB | `usage.BranchCost`: numbers only |

### 4.13 New inputs: who controls it, bound, cleaner, sink

| Input | Controlled by | Bound | Cleaned by | Reaches |
| --- | --- | --- | --- | --- |
| ingest document | any local process: cost-lens, Curia's status line, an agent | 64 KiB; section 4.2's ranges | `usage.ParseDoc` (numbers, RFC 3339 times, one id pattern) | SQL parameters; numbers Curia formats itself |
| status-line document | Claude Code | 64 KiB | `usage.FromStatusLine`; the model name through `sanitize.Line(…, 24)` | SQL parameters; one line on Claude Code's status line |
| `settings.json`'s `statusLine` | me | the file as `loadSettings` reads it | compared, never executed | left alone or replaced by Curia's own |
| analyzer path, `node` | config, `PATH` | 1024 bytes | absolute, suffix and regular-file checks | an argv, no shell |
| analyzer output | cost-lens, from Claude Code's logs | 4 MiB, 90 s | `usage.BranchCost`: finite numbers in range; the branch name only compared | one number in the review |
| Obsidian folder | config | 1024 bytes | absolute; `os.OpenRoot` | every write stays under it |
| file names and first 20 lines of files in that folder | me, my vault | 4096 entries read, 4 KiB per file | name and marker match only | a decision to overwrite or leave |
| decision text into a note or an entry | agents and me | 64 KiB a note; section 4.8's cuts | `sanitize.Text`, `export.Fence`, JSON quoting | a file in my vault; stdout |
| `CURIA_LINEAR_API_KEY` | the environment | 200 bytes, `[A-Za-z0-9_.-]` | pattern check | one request header to `linear.url` |
| Linear URL, team key | config, a flag (the key only) | 200 bytes; 10 | `config.Load`; pattern | the HTTP client; a JSON variable |
| Linear response | Linear | 4 MiB a page, 4 pages | strict field types; `sanitize.Line`, `sanitize.Block`; id and url patterns | the store, as task text |
| Codex hook payload | Codex | 1 MiB, as every hook payload | `hook.Parse`, unchanged | the session row |
| Codex hooks file and `config.toml` | me, Codex | 1 MiB | JSON and TOML decoding; entries compared | Curia's entries added or removed; the rest kept byte-equal in meaning |
| `--priority`, `--agent` | me, an agent (`task`, `spawn` are agent-facing) | 2 runes; 6 | enum match | SQL parameters; which program `launch` runs, from a list of two |

## 5. File structure

Module `github.com/ZachFara/curia`. Three packages are added. The package order (each may import only those above it) becomes: `model`, `config`, `sanitize`, `decision`, `precedent`, `task`, `message`, `route`, `team`, **`usage`**, **`export`**, **`linear`**, `store`, and the rest as Plan 4 leaves them.

| Path | Responsibility | Task |
| --- | --- | --- |
| `internal/model/` | Plus `usage.go` (`UsageWindow`, `UsageSample`, `CostSample`, priority and agent constants); `Session` gains cost and cache fields; `Event.Agent`; `Task.Priority`, `Task.Agent`, `SourceLinear`; `Decision.ExportedAt`. | 1 |
| `internal/config/` | Plus `Usage`, `Obsidian`, `Linear`, `CostLens`. | 1 |
| `internal/usage/` **new** | Pure rules: the ingest contract, the status-line mapping and text, the cost-lens summary reader (`ingest.go`, `costlens.go`); burn, forecast, attribution, the pause suggestion, the cost and cache cells (`forecast.go`). No I/O. | 2, 3 |
| `internal/export/` **new** | Pure rules: the Obsidian note (name, fence, content, marker) and the Agent Commons entry. No I/O. | 4 |
| `internal/linear/` **new** | The read-only Linear client: one query over `net/http`. | 5 |
| `internal/store/` | Plus migration `0006_cost.sql`, `usage.go` (ingest and sample reads); `store.go` reads the cost and cache columns; `tasks.go` and `decisions.go` carry priority, agent and the export mark. | 6, 7 |
| `internal/state/`, `internal/hook/` | The agent of a session; no transcript read and no Stop claim for Codex. | 8 |
| `internal/install/` | Plus `statusline.go` and `codex.go`. | 9, 10 |
| `internal/notify/` | `DecisionPush` takes the stakes line. | 11 |
| `internal/fleet/` | `Counts.Forecast`; `Unit` cost and cache. | 12 |
| `internal/task/` | Priority in `Normalize` and `Validate`; `ParsePriority`, `PriorityLabel`. | 15 |
| `internal/ui/` | The dashboard's `COST` and `CACHE` columns, burn sort, pause suggestion; the review's branch cost. | 19, 20, 21 |
| `cmd/curia/` | New files `ingest.go`, `statusline.go`, `usage.go`, `export.go`, `costlens.go`; changes to `install.go`, `push.go`, `hook.go`, `status.go`, `task.go`, `task_import.go`, `spawn.go`, `launch.go`, `decide.go`, `review.go`, `ui.go`, `pause.go`. | 8 to 11, 13 to 21 |
| `docs/ingest-schema.json` **new** | The ingest contract as a JSON Schema (spec F15: it lives in Curia's repository). | setup |
| `testdata/cost/` **new** | Hand-written documents, payloads and stubs for the acceptance test. | 22 |
| `e2e/` | Plus `cost_test.go` and its golden files. | 22 |

## 6. Setup (orchestrator, before the first task)

One PR from `chore/plan5-setup` into `main`, after `plan-3` and `plan-4` have merged, the dependency table has been checked, and the probes of section 13 that are not the owner's have run; `plan-5` is branched from `main` once it merges. No task touches these files afterwards.

1. **`go.mod`, `go.sum`, `Makefile`, `lefthook.yml`, `.goreleaser.yaml`:** no change. No module is added.
2. **`.golangci.yml` (depguard):** three rules, in the form of the existing ones. `internal/usage` may import `model`, `config`, `sanitize`, `decision`, `precedent`, `task`, `message`, `route`, `team` and itself; `internal/export` those and `usage`; `internal/linear` those and `export`. Every package from `store` down gains the three in its allow list. `internal/linear` is the only one of the three that may import `net/http`; no rule is needed for that beyond review, since depguard here governs internal imports.
3. **`scripts/check-layout.sh`:** `internal/usage`, `internal/export`, `internal/linear` join the allowed packages; `CHANGELOG.md` joins the allowed root files (written by Task 23). `testdata/cost/`, `e2e/testdata/cost/` and the packages' own `testdata/` sit in allowed places.
4. **`.github/workflows/ci.yml`:** add `plan-5` to `branches`.
5. **`cmd/curia/main_test.go`:** `guardUnset` gains `CURIA_LINEAR_API_KEY` and `CODEX_HOME`. Three seams start real programs: `codexRunner` (Task 10), `costlensLookPath` and `costlensRun` (Task 19); each is declared by its own task, and that task's test file replaces it with a default that fails any test which has not set its own, as Plan 2 required of `claudeRunner`.
6. **`docs/ingest-schema.json`:** a JSON Schema (draft 2020-12) for section 4.2: `type: object`, `required: ["schema_version", "session_id"]`, `additionalProperties: true`, each property with its type, pattern, minimum and maximum, and the spec's example under `examples` with `at` added. Task 2's `TestSchemaFileMatchesParser` holds the file and the parser together.
7. **`AGENTS.md`:**
   - "Sources of truth": this plan is the current plan; Plans 3 and 4 are complete.
   - "What Curia is": one sentence each for `curia ingest` and what it writes, the status line, the forecast in the badge and `curia usage`, the pause suggestion and the cheaper-model default (never automatic), `curia export`, `curia task import --linear`, Codex sessions.
   - Directory layout: the three packages, `docs/ingest-schema.json`, `testdata/cost/`.
   - Toolchain rule: "during Plans 1 to 5". Conventions: Plan 5 task branches are `feat/plan5-tN-<slug>`, worktrees `.worktrees/plan5-tN-<slug>`; a task merges when its named tests and `make check` pass and CI is green, and it is one commit; the orchestrator starts a task the moment every task on its **Needs** line has merged.
   - "Things learned while building", new entries: `window` is usable as a SQLite column name (3.51.0, at the desk); cost-lens's analyzer is `node <package>/analyzer/cli.js --project <repo>`, found beside the `cost-lens` link's target, takes about 12 seconds and prints `currentProject.branches[].spend.cost` in four parts; cost-lens 0.3.0 has no push to `curia ingest` yet, so the status line is the live source until it does; Codex 0.166.0 has `hooks` on by default and needs hook trust; ingest never touches `updated_at`; `os.Root` is how the Obsidian folder is written; whatever section 13's probes establish, each with its Claude Code, Codex and cost-lens version.
8. **Decision records:** `docs/decisions/0025-ingest-contract-and-samples.md` (sections 4.1, 4.2, 4.3); `0026-forecast-is-advice.md` (section 4.4, decisions 8 to 14); `0027-one-way-exports.md` (sections 4.7, 4.8); `0028-linear-read-only.md` (section 4.9); `0029-codex-as-second-agent.md` (section 4.10). If Plans 3 and 4 used other numbers, these take the next free ones.
9. **Fixtures recorded from the real programs,** committed by setup and never generated by a task: `internal/usage/testdata/statusline-captured.json` (probe S1); `internal/usage/testdata/summary-captured.json` (the analyzer's real output for a throwaway repository, with titles, paths and branch names replaced by placeholders); `internal/hook/testdata/codex/*.json` and `internal/install/testdata/codex/` (probes X1, X2), when the owner has approved X1; `internal/linear/testdata/issues-captured.json` (probe L1), when the owner has run it. A fixture that is missing because its probe is the owner's does not hold up setup: the tasks carry built fixtures (section 13).
10. **`PROGRESS.md`:** a Plan 5 section: the task table of section 7 with done criteria and each task's **Needs** line, a ledger, Rulings from N (the first number free after Plan 4's last ruling), the probes' results, the owner's questions of section 15. Mark as addressed what section 14 lists as picked up.
11. **Migration numbering:** `0006_cost.sql` is this plan's only migration and Task 6 creates it (see E1). A migration found necessary later is created by the orchestrator in a fix wave.

`README.md` and `CHANGELOG.md` are not part of setup: Task 23 writes them when everything has merged.

## 7. Tasks, order and models

Each task runs on its own branch and worktree and merges by PR into `plan-5`. The orchestrator does not wait for waves: it starts a task the moment every task on its **Needs** line has merged. The wave number below is only the earliest point at which a task can start, and the appendix checks it. Counts in brackets are files, tests and fixture directories included.

| Task | Model | Needs | Touches | Wave |
| --- | --- | --- | --- | --- |
| 1 Types and config | Sonnet | none | `internal/model/usage.go` (new), `model.go`, `task.go`, `decision.go`, `internal/config/config.go`, `config_test.go` [6] | 1 |
| 2 Usage: ingest contract, status line, cost-lens summary | Sonnet | 1 | `internal/usage/ingest.go`, `ingest_test.go`, `costlens.go`, `costlens_test.go`, `testdata/` (all new) [5] | 2 |
| 3 Usage: forecast, burn, cells | Opus | 1 | `internal/usage/forecast.go`, `forecast_test.go` (new) [2] | 2 |
| 4 Export rules | Sonnet | none | `internal/export/obsidian.go`, `commons.go`, `export_test.go` (new) [3] | 1 |
| 5 Linear client | Opus | none | `internal/linear/linear.go`, `linear_test.go`, `testdata/` (new) [3] | 1 |
| 6 Store: ingest and samples | Opus | 1 | `internal/store/migrations/0006_cost.sql`, `usage.go`, `usage_test.go` (new), `store.go`, `store_test.go` [5] | 2 |
| 7 Store: priority, agent, export mark | Sonnet | 6 | `internal/store/tasks.go`, `tasks_test.go`, `decisions.go`, `decisions_test.go` [4] | 3 |
| 8 Hook and state: the session's agent | Opus | 1 | `internal/state/state.go`, `state_test.go`, `internal/hook/run.go`, `hook_test.go`, `testdata/codex/` (new), `cmd/curia/hook.go`, `hook_test.go` [7] | 2 |
| 9 Install: status line | Opus | none | `internal/install/statusline.go`, `statusline_test.go` (new), `cmd/curia/install.go`, `install_test.go` [4] | 1 |
| 10 Install: Codex | Opus | 9, and probes X1 and X2 | `internal/install/codex.go`, `codex_test.go`, `testdata/codex/` (new), `cmd/curia/install.go`, `install_test.go` [5] | 2 |
| 11 Push: the stakes line | Sonnet | none | `internal/notify/ntfy.go`, `ntfy_test.go`, `cmd/curia/push.go`, `push_test.go` [4] | 1 |
| 12 Fleet: forecast in the badge, cost and cache of a unit | Sonnet | 1 | `internal/fleet/fleet.go`, `fleet_test.go`, `units.go`, `units_test.go` [4] | 2 |
| 13 `curia ingest`, `curia statusline` | Opus | 2, 6 | `cmd/curia/ingest.go`, `ingest_test.go`, `statusline.go`, `statusline_test.go` (new) [4] | 3 |
| 14 `curia status` forecast, `curia usage` | Sonnet | 3, 6, 12 | `cmd/curia/status.go`, `status_test.go`, `usage.go`, `usage_test.go` (new) [4] | 3 |
| 15 Task priority | Sonnet | 7 | `internal/task/task.go`, `task_test.go`, `cmd/curia/task.go`, `task_test.go`, `cmd/curia/testdata/tasks.golden.json` [5] | 4 |
| 16 Linear import | Opus | 5, 7 | `cmd/curia/task_import.go`, `task_import_test.go` [2] | 4 |
| 17 Spawn and launch: agent and cheaper model | Opus | 3, 6, 7 | `cmd/curia/spawn.go`, `spawn_test.go`, `launch.go`, `launch_test.go` [4] | 4 |
| 18 `curia export` | Sonnet | 4, 7 | `cmd/curia/export.go`, `export_test.go` (new), `decide.go`, `decide_test.go` [4] | 4 |
| 19 Review: branch cost | Opus | 2 | `cmd/curia/costlens.go`, `costlens_test.go` (new), `review.go`, `review_test.go`, `internal/ui/review.go`, `review_view.go`, `review_test.go` [7] | 3 |
| 20 Dashboard: cost, cache, burn sort | Opus | 3, 6, 12 | `internal/ui/model.go`, `view.go`, `ui_test.go`, `cmd/curia/ui.go`, `ui_test.go` [5] | 3 |
| 21 Dashboard: pause suggestion | Opus | 7, 14, 20 | `internal/ui/model.go`, `view.go`, `ui_test.go`, `cmd/curia/ui.go`, `ui_test.go`, `pause.go` [6] | 4 |
| 22 Acceptance | Opus | 1 to 21 (10 may be pending: see the task) | `testdata/cost/`, `e2e/cost_test.go`, `e2e/testdata/cost/` (new) [3] | 5 |
| 23 V1 docs and release notes | orchestrator | 22 | `README.md`, `CHANGELOG.md`, `AGENTS.md`, `PROGRESS.md`, `docs/` | after 5 |

**The shape of the graph.** Five tasks need nothing (1, 4, 5, 9, 11) and seven more need only Task 1 or Task 9. The longest chain is four tasks (1, 6, 7, then 15, 16, 17 or 18). **Hot files, and the order their editors merge in:** `cmd/curia/install.go` and `install_test.go`: Task 9, then Task 10. `internal/ui/model.go`, `view.go`, `ui_test.go`, `cmd/curia/ui.go` and `ui_test.go`: Task 20, then Task 21. Every other file has one editor in this plan. Tasks 7, 13, 14, 19 and 20 all become ready when Task 6 (with 2, 3 or 12) merges and touch disjoint files.

**Why each model.** Sonnet: 1 (types and validation); 2 and 4 (pure rules with exact signatures and table tests); 7 (column plumbing through existing statements); 11 (one more argument through two files); 12 (pure); 14 and 15 (thin commands over finished rules and store calls); 18 (a command over pure rules and `os.Root`, with the file-system cases named as tests). Opus: 3 (the forecast's edge cases are judgment: resets, sparse samples, merged sessions); 5 and 16 (the network, a secret); 6 (a write transaction that must coexist with concurrent hooks); 8 (the hook path); 9 and 10 (files I own in another program's configuration: Plan 2's install task needed Opus for the same reason); 13 (a process boundary with a latency budget and a never-fail rule); 17 and 19 (process boundaries: `launch`, `node`); 20 and 21 (UI: Plan 2's decide view took four fix rounds on key handling); 22 (cross-process acceptance).

**Where scope was kept down.** Twenty-two implementer tasks, the same as Plan 4, for three features and everything handed forward, one commit each. No MCP tool, no tmux key, option or format, no new module. One ingest path serves cost-lens and the status line. Curia computes no price and reads no token counter: attribution is in dollars. The Agent Commons entry is printed, not sent, so there is no second network client and no stored credential. Linear is one query with a fixed filter; no state mapping, no pagination beyond 200 issues, no sync command. Codex is tracked, spawned and resumed through `launch`; it is not typed into, not handed answers at Stop, not restored when unlinked, and gets no transcript parser. The forecast is not in the board, `curia ls` is unchanged, and `ls --json` gains no key (`curia usage --json` carries cost and burn). No config key that nothing reads: four sections, five keys. Each cut is recorded in sections 12, 14 and 16.

**Smoke and live checks** (orchestrator, on `plan-5`, temp `HOME` and `CURIA_DB`, a private tmux server):

| When | Check |
| --- | --- |
| Tasks 6 and 7 merged | `make check`; open the new database with the Plan 4 binary's `curia ls`, `curia task ls` and `curia hook Stop`, and the reverse: all work, and a Plan 4 `task edit` leaves `priority` alone |
| Tasks 13 and 14 merged | by hand: three `curia ingest` documents with `at` 15, 8 and 1 minutes ago; `curia status` shows the warning; `curia usage` lists the windows; `echo garbage \| curia ingest; echo $?` prints `0` |
| Tasks 9, 13, 20 merged | **live check A** (real Claude Code as section 13 prescribes, the status line registered through the temp settings file): after two turns `usage_samples` and the session's cost are filled; the status line reads as section 4.3; the dashboard shows `COST`; a blocking ticket's popup shows no stakes line (cache unknown without cost-lens), and after a hand-made ingest with `cache_expires_at` it counts down |
| Tasks 17, 18, 19, 21 merged | **live check B:** under a hand-made warning series, `curia spawn` starts the stub-free real agent with `--model sonnet` and says why; `P` then `y` in the real popup pauses the suggested task, and a held `y` or a paste does not; `curia decide` writes a note into a throwaway folder; the review shows the branch cost from the real analyzer for a branch worked in a sibling worktree (probe C2's result) |
| Task 22 merged | `deadcode ./...` without `-test`: only test doubles are reported; then Task 23 |

## 8. Tasks 1, 4, 5, 9 and 11 (need nothing)

### Task 1: Types and config

**Model:** Sonnet (types and validation, exact signatures).

**Needs:** tasks: none. **Touches:** `internal/model/usage.go`, `model.go`, `task.go`, `decision.go`, `internal/config/config.go`, `config_test.go`.

**Files:**

- Create: `internal/model/usage.go`
- Modify: `internal/model/model.go`, `internal/model/task.go`, `internal/model/decision.go`, `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Brief includes:** section 4.11.

**Interfaces:**

- Consumes: `model.Session`, `model.Event`, `model.Task`, `model.TaskSource`, `model.Decision` (E3).
- Produces (types and constants; the only logic is in `config`):

```go
// internal/model/usage.go
type UsageWindow string // WindowFiveHour "five_hour", WindowSevenDay "seven_day"
// UsageSample is one reading of an account-wide usage window.
type UsageSample struct {
    N         int64
    At        time.Time
    Window    UsageWindow
    UsedPct   float64
    ResetsAt  time.Time // zero: the reading had none
    SessionID string
}
// CostSample is one reading of a session's running cost.
type CostSample struct {
    N         int64
    At        time.Time
    SessionID string
    CostUSD   float64
}
const (
    PriorityHigh   = 1 // P1
    PriorityNormal = 2 // P2, the default
    PriorityLow    = 3 // P3
)
const (
    AgentClaude = "claude"
    AgentCodex  = "codex"
)
```

  - `model.Session` gains `CostUSD float64`, `CostKnown bool` (false: `cost_usd` is NULL), `CacheExpiresAt time.Time` (zero: unknown), `CacheRebuildUSD float64`. They are read from the store and written only by `store.Ingest`.
  - `model.Event` gains `Agent string`: "the agent that sent the event; empty means Claude Code".
  - `model.Task` gains `Priority int` (1 to 3; a zero value means 2) and `Agent string` (empty means `claude`); `SourceLinear TaskSource = "linear"`.
  - `model.Decision` gains `ExportedAt time.Time`.
  - `config.Config` gains `Usage config.Usage` (`toml:"usage"`), `Obsidian config.Obsidian` (`toml:"obsidian"`), `Linear config.Linear` (`toml:"linear"`), `CostLens config.CostLens` (`toml:"costlens"`):

```go
type Usage struct {
    CheaperModel string `toml:"cheaper_model"` // default "sonnet"; "" turns the spawn default off
}
type Obsidian struct {
    Dir string `toml:"dir"` // "~" expanded; "" means no export
}
type Linear struct {
    URL  string `toml:"url"`  // default DefaultLinearURL
    Team string `toml:"team"`
}
type CostLens struct {
    Analyzer string `toml:"analyzer"` // "~" expanded
}
const DefaultLinearURL = "https://api.linear.app/graphql"
```

  `Load` applies section 4.11's rules, each error naming its key (`usage.cheaper_model`, `obsidian.dir`, `linear.url`, `linear.team`, `costlens.analyzer`). A `cheaper_model` set to `""` in the file stays empty; an absent key gets the default.

- [ ] **Step 1: Write the failing tests**

```go
func TestPlan5Defaults(t *testing.T)            // Default(): cheaper_model "sonnet", obsidian dir "", linear URL DefaultLinearURL and team "", analyzer ""
func TestLoadPlan5Overrides(t *testing.T)       // every key of section 4.11 set -> read back; dir "~/Vault/Decisions" and analyzer "~/x/cli.js" expanded under the test's HOME
func TestCheaperModelEmptyTurnsItOff(t *testing.T) // cheaper_model = "" in the file -> "", not the default
func TestBadUsageConfigRejected(t *testing.T)   // cheaper_model "son net", "a;b", 65 runes -> error naming usage.cheaper_model
func TestBadObsidianDirRejected(t *testing.T)   // "Vault/Decisions" (relative), a 1025-byte path, a path holding "\n" -> error naming obsidian.dir
func TestBadLinearConfigRejected(t *testing.T)
// url "ftp://x", "https://", "https://u:p@api.linear.app/graphql", "https://api.linear.app/graphql?x=1", "http://example.com/graphql", 201 bytes;
// team "eng", "ENG-1", "1ENG", 11 runes -> error naming the key; "http://127.0.0.1:9000/graphql" and "http://localhost:9000/graphql" accepted
func TestBadAnalyzerRejected(t *testing.T)      // "cli.js" (relative), "/x/cli.sh", "/x/cli.js\x1b" -> error naming costlens.analyzer; "/x/cli.js" and "/x/cli.ts" accepted
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/config/ -v`. Expected: FAIL, unknown field `Usage`.
- [ ] **Step 3: Implement.** `TestDefaults` gains the Plan 5 assertions. URL checks use `net/url` and `net/netip`, as Plan 4's do.
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/model/ ./internal/config/ -v && go build ./...`. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat: usage, cost and export types; usage, obsidian, linear and costlens config"`

**Done when:** the seven tests pass, every existing config test still passes, `go build ./...` works.

### Task 4: Export rules

**Model:** Sonnet (pure rules, exact signatures, table tests).

**Needs:** tasks: none. **Touches:** `internal/export/obsidian.go`, `commons.go`, `export_test.go`.

**Files:**

- Create: `internal/export/obsidian.go`, `internal/export/commons.go`
- Test: `internal/export/export_test.go`

**Brief includes:** sections 4.7 (Name, Finding an existing note, Content) and 4.8, and section 3's item 5.

**Interfaces:**

- Consumes: `model.Decision`, `model.Option`, the status and scope constants (`internal/model/decision.go`, as in `main`); `decision.IsDecided`; `sanitize.Line`, `sanitize.Text`.
- Produces (all pure):

```go
package export

const (
    MaxNote    = 64 << 10 // bytes of one note
    MaxTitle   = 60       // runes of a note's title
    MaxContext = 8 << 10  // bytes of the context section
)

// NoteTitle is the question as a plain file-name-safe title; "" when nothing is left.
func NoteTitle(question string) string
// NoteName is the file name for d: "<id> <title>.md", or "<id>.md".
func NoteName(d model.Decision) string
// IsNoteOf reports whether a file name is a note of decision id: "<id>.md", or "<id> ….md".
func IsNoteOf(name, id string) bool
// OwnsNote reports whether head, the first bytes of a file, holds the front-matter line "curia_id: <id>" within its first 20 lines.
func OwnsNote(head []byte, id string) bool
// Fence wraps text in a code fence that text cannot close: a run of backticks one longer than the longest in text, at least three, no info string.
func Fence(text string) string
// Note is the whole note for a decided decision.
func Note(d model.Decision) []byte

// Entry is the argument object of Agent Commons's propose tool, without the proposing agent's own fields.
type Entry struct {
    Symptom     string            `json:"symptom"`
    Environment map[string]string `json:"environment"`
    Cause       string            `json:"cause"`
    Fix         string            `json:"fix"`
    Domains     []string          `json:"domains"`
}
// ErrNotPublishable is returned for a decision that is not decided, is superseded, or is not of global scope.
var ErrNotPublishable = errors.New("export: not a decided global decision")
func CommonsEntry(d model.Decision) (Entry, error)
```

Rules: sections 4.7 and 4.8, exactly. `Note` ends with one newline; `status` is `superseded` when `d.SupersededAt` is set; the chosen option is the one whose key equals `d.AnswerKey`; a free-text answer marks no option; a standing rule (no options) has no `## Options` section. `IsNoteOf("D-11 x.md", "D-1")` is false: the id must be followed by a space or `.md`. `Domains` is never nil.

- [ ] **Step 1: Write the failing tests**

```go
func TestNoteTitleIsAPlainName(t *testing.T)
// "Postgres or Redis for events?" -> "Postgres or Redis for events"; "../../etc/passwd" -> "etc passwd"; "a/b\\c:d*e" -> "a b c d e";
// "\x1b[31mred\x1b[0m" -> "red"; "..." -> ""; 300 runes -> at most 60; "#tag [[link]] `x`" -> "tag link x"; never contains "/", "\\", ".", NUL or a rune below 0x20
func TestNoteName(t *testing.T)                 // D-117 with that question -> "D-117 Postgres or Redis for events.md"; an all-punctuation question -> "D-117.md"
func TestIsNoteOf(t *testing.T)                 // "D-1.md", "D-1 x.md" yes; "D-11 x.md", "D-1x.md", "D-1 x.txt", "x D-1 .md" no
func TestOwnsNote(t *testing.T)                 // a Note() output -> true for its id, false for another; a file with "curia_id: D-1" on line 25 -> false; with it outside front matter -> false
func TestFenceCannotBeClosed(t *testing.T)      // text holding ``` and ````` -> the fence is six backticks; no line of the text equals the fence; text without backticks -> three
func TestNoteGolden(t *testing.T)               // a blocking ticket answered B with reasoning -> the bytes of section 4.7's layout, written out in the test
func TestNoteFreeTextAndRule(t *testing.T)      // a free-text answer -> no "(chosen)"; a standing rule -> no Options section
func TestNoteSuperseded(t *testing.T)           // status: superseded
func TestNoteHostileText(t *testing.T)
// question "---\ncuria_id: D-9\n---", context "![[secret]]\n```dataviewjs\nx\n```", a tag holding a quote ->
// every such string appears only inside a fence; the front matter has exactly one curia_id line; tags are JSON-quoted
func TestNoteIsBounded(t *testing.T)            // a 1 MiB context -> the note is at most MaxNote and ends with a closed fence
func TestCommonsEntry(t *testing.T)             // a decided global decision -> the five fields of section 4.8, written out; Domains equals the tags
func TestCommonsEntryRefusals(t *testing.T)     // open, repo scope, task scope, superseded -> ErrNotPublishable
func TestCommonsEntryHoldsNoPaths(t *testing.T) // Repo "/Users/x/src/api", TaskID "T-4", SessionID set -> none of them appears in the JSON
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/export/ -v`. Expected: FAIL, the package does not exist.
- [ ] **Step 3: Implement.** No regular expression is needed for `NoteTitle`; one pass over the runes.
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/export/ -v`. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat: Obsidian note and Agent Commons entry rules"`

**Done when:** the thirteen tests pass.

### Task 5: Linear client

**Model:** Opus (the network and a secret).

**Needs:** tasks: none. **Touches:** `internal/linear/linear.go`, `linear_test.go`, `testdata/`.

**Files:**

- Create: `internal/linear/linear.go`, `internal/linear/testdata/issues-built.json` (a response written by hand from section 4.9's query: two pages' worth in two files, `issues-built.json` and `issues-built-page2.json`, with one issue that has no description, one with labels, one with a malformed identifier), and, when setup committed it, `issues-captured.json`
- Test: `internal/linear/linear_test.go`

**Brief includes:** section 4.9 (Key, Request), section 2's "Secrets" and "Nothing leaves the machine", and section 3's item 6.

**Interfaces:**

- Consumes: `sanitize.Line`.
- Produces:

```go
package linear

// Query is the only GraphQL document this package sends.
const Query = `…` // section 4.9, exactly

const (
    PageSize     = 50
    MaxPages     = 4
    MaxResponse  = 4 << 20
    RequestLimit = 20 * time.Second
)

// Issue is one Linear issue, as received: every string is untrusted.
type Issue struct {
    Identifier, Title, Description, URL, StateType string
    Priority                                       int
    Labels                                         []string
}

// ValidKey reports whether k can be a Linear API key: 1 to 200 bytes of [A-Za-z0-9_.-].
func ValidKey(k string) bool
// ValidTeam reports whether k is a team key: ^[A-Z][A-Z0-9]{0,9}$.
func ValidTeam(k string) bool

// Client reads issues from one Linear endpoint. It never writes.
type Client struct {
    URL  string       // the GraphQL endpoint
    Key  string       // sent as the Authorization header and nowhere else
    HTTP *http.Client // nil: a client that follows no redirect
}
// Issues returns the issues assigned to the key's user in backlog or unstarted states, of team when it is not empty, newest update first, at most MaxPages pages.
func (c Client) Issues(ctx context.Context, team string) ([]Issue, error)
```

Rules: section 4.9's request, exactly. The body is built with `encoding/json` from a struct, never by concatenation. Each request is bounded by `RequestLimit` when `ctx` has no earlier deadline; the default client's `CheckRedirect` returns `http.ErrUseLastResponse` and a 3xx is an error. The response is read through `io.LimitReader(MaxResponse + 1)`; one byte over is an error. It is decoded into typed structs; a `nodes` entry of the wrong shape is skipped, not fatal. Errors: `linear: status 401`, `linear: <first error message through sanitize.Line(…, 200)>`, `linear: response too large`, or the transport error's kind (`timeout`, `connection refused`), never the URL's query, a header or the key. Before any error is returned, every occurrence of the key in its text is replaced by `<key>`, as a last guard. `Issues` with an invalid key or team returns an error without sending anything.

- [ ] **Step 1: Write the failing tests** (a local `httptest.Server` stands in for Linear)

```go
func TestClientReadsBuiltPages(t *testing.T)        // the server serves the two built files by cursor -> the issues in order, labels read, the description-less one with ""
func TestClientReadsCapturedResponse(t *testing.T)  // issues-captured.json when present (t.Skip with the probe's name when the file is absent)
func TestClientSendsOnlyTheQuery(t *testing.T)
// every request: POST, Content-Type application/json, Authorization equal to the key, User-Agent curia; the body's "query" equals linear.Query byte for byte;
// "variables" holds first 50, the cursor, and the filter of section 4.9; the word "mutation" appears nowhere in the body or in linear.Query
func TestClientTeamFilter(t *testing.T)             // team "ENG" -> filter.team.key.eq "ENG"; team "" -> no team key
func TestClientStopsAtMaxPages(t *testing.T)        // a server that always says hasNextPage -> exactly 4 requests
func TestClientDoesNotFollowRedirect(t *testing.T)  // 302 to a second listener -> an error; the second listener saw no request (so no Authorization header)
func TestClientErrors(t *testing.T)                 // 401, 500, a body {"errors":[{"message":"x\x1b[2J"}]}, invalid JSON, 5 MiB -> the errors of the rules, cleaned
func TestErrorsNeverHoldTheKey(t *testing.T)        // a server that echoes the Authorization header into its error message -> the returned error holds "<key>" and not the key
func TestClientTimeout(t *testing.T)                // a server that never answers, ctx with 50 ms -> an error within 1 s
func TestInvalidKeyOrTeamSendsNothing(t *testing.T) // key "a b", team "eng" -> an error; the server saw no request
func TestValidKeyAndTeam(t *testing.T)
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/linear/ -v`. Expected: FAIL, the package does not exist.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/linear/ -v`. Expected: PASS; `TestClientReadsCapturedResponse` skipped only while the fixture is absent.
- [ ] **Step 5: Commit.** `git commit -m "feat: read-only Linear issue client"`

**Done when:** the eleven tests pass and `grep -ri mutation internal/linear/*.go` finds only the test's assertion.

### Task 9: Install: status line

**Model:** Opus (a file I own in Claude Code's configuration).

**Needs:** tasks: none. **Touches:** `internal/install/statusline.go`, `statusline_test.go`, `cmd/curia/install.go`, `install_test.go`. Merges before Task 10.

**Files:**

- Create: `internal/install/statusline.go`
- Modify: `cmd/curia/install.go`
- Test: `internal/install/statusline_test.go`, `cmd/curia/install_test.go`

**Brief includes:** section 4.3 (the three install items), section 2's "Installer rulings", and section 3's item 7.

**Interfaces:**

- Consumes: `loadSettings`, `saveSettings`, `shellQuote` (`internal/install/settings.go`); `executablePath`, `installSay`, `installPaths` (`cmd/curia/install.go`, E14).
- Produces:

```go
// StatusLineState is what StatusLine found or did.
type StatusLineState int
const (
    StatusLineAdded   StatusLineState = iota // Curia's status line was written
    StatusLineOurs                           // it was already there
    StatusLineForeign                        // the user has their own; nothing was written
)
// StatusLineCommand is the command Curia registers for bin.
func StatusLineCommand(bin string) string // shellQuote(bin) + " statusline"
// StatusLine registers Curia's status-line command in settingsPath unless a statusLine is already set.
func StatusLine(settingsPath, bin string, now time.Time) (StatusLineState, error)
// RemoveStatusLine deletes the statusLine key when its command is Curia's for bin, and reports whether it did.
func RemoveStatusLine(settingsPath, bin string) (bool, error)
```

  - `curia install` gains `--no-statusline`; without it, the step runs after the hooks and prints section 4.3's line for the state. `curia uninstall` calls `RemoveStatusLine` and prints `status line: removed` when it did.

Rules. "Ours" means `statusLine` is an object whose `type` is `command` and whose `command` equals `StatusLineCommand(bin)`. A settings file that does not exist is created with the one key, as `Hooks` does. The backup, the atomic rename and the symlink rule are `saveSettings`'s. A `StatusLineForeign` or `StatusLineOurs` result writes nothing, so it makes no backup. Every other key of the file is preserved.

- [ ] **Step 1: Write the failing tests**

```go
// internal/install/statusline_test.go
func TestStatusLineAddsWhenAbsent(t *testing.T)         // settings with hooks and permissions -> state Added; the key equals {"type":"command","command":"'<bin>' statusline"} as shellQuote gives it; every other key unchanged; one backup
func TestStatusLineCreatesMissingFile(t *testing.T)
func TestStatusLineIdempotent(t *testing.T)             // a second call -> Ours, the file byte-equal, no second backup
func TestStatusLineForeignUntouched(t *testing.T)       // a user's {"type":"command","command":"~/bin/sl.sh"}; a string value; an array; null -> Foreign, the file byte-equal, no backup
func TestStatusLineOtherBinaryIsForeign(t *testing.T)   // Curia's command for another path -> Foreign (uninstall with that binary removes it)
func TestRemoveStatusLineOnlyOurs(t *testing.T)         // ours -> removed, other keys kept; foreign -> false, byte-equal; absent -> false
func TestStatusLineBinWithSpaces(t *testing.T)          // bin "/Users/a b/curia" -> one shell word
func TestStatusLineSymlinkedSettingsSurvive(t *testing.T) // settings.json is a symlink into another directory -> still a symlink, its target rewritten
// cmd/curia/install_test.go
func TestInstallRegistersStatusLine(t *testing.T)       // curia install into the temp home -> the key is set; the output holds the "registered" line
func TestInstallNoStatusLineFlag(t *testing.T)          // --no-statusline -> no key, no line about it
func TestInstallLeavesForeignStatusLine(t *testing.T)   // the output holds the "you have your own" line with the tee hint
func TestUninstallRemovesOnlyOurStatusLine(t *testing.T)
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/install/ ./cmd/curia/ -run 'StatusLine|Statusline' -v`. Expected: FAIL, undefined `StatusLine`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/install/ ./cmd/curia/ -v`. Expected: PASS, every existing install test included.
- [ ] **Step 5: Commit.** `git commit -m "feat: register a status-line command that feeds usage to Curia"`

**Done when:** the twelve tests pass and no existing install test changed its assertions, except that a test which compares a whole installed `settings.json` gains the one key.

### Task 11: Push: the stakes line

**Model:** Sonnet (one more argument through two files).

**Needs:** tasks: none. **Touches:** `internal/notify/ntfy.go`, `ntfy_test.go`, `cmd/curia/push.go`, `push_test.go`.

**Files:**

- Modify: `internal/notify/ntfy.go`, `cmd/curia/push.go`
- Test: `internal/notify/ntfy_test.go`, `cmd/curia/push_test.go`

**Brief includes:** section 4.5's "Push" item.

**Interfaces:**

- Consumes: `notify.DecisionPush`, `pushFlush` (E12); `decision.Stakes(t model.Ticket, now time.Time) string` and `store.OpenTickets(ctx context.Context, now time.Time) ([]model.Ticket, error)` (as in `main`).
- Produces: `func DecisionPush(d model.Decision, topic, answerURL, token, stakes string) Notification`: "stakes, when not empty, is the message's last line". The line goes through `sanitize.Line(…, 120)` and is appended before the whole message is cut to 1200 runes; when the cut would remove it, the question is shortened instead so the stakes line stays. `pushFlush`, for a decision push it is about to publish, finds the ticket among `st.OpenTickets(ctx, now())` by id and passes `decision.Stakes(ticket, now())`; a ticket it does not find there has an empty stakes line. Nothing else in `push.go` changes.

- [ ] **Step 1: Write the failing tests**

```go
// internal/notify/ntfy_test.go
func TestDecisionPushStakesLine(t *testing.T)        // stakes "answer within 12m to keep cache warm (rebuild ≈ $1.68)" -> the message's last line; "" -> the message of Plan 4, byte for byte
func TestDecisionPushStakesSurvivesTheCut(t *testing.T) // a 1500-rune question -> the message is at most 1200 runes and still ends with the stakes line
// cmd/curia/push_test.go
func TestFlushAddsStakesForWarmCache(t *testing.T)   // the asking session has cache_expires_at 12 minutes ahead and a rebuild cost (set by raw SQL in the test) -> the stand-in ntfy got the warm line
func TestFlushAddsColdLine(t *testing.T)             // an expiry in the past, context_used 84000 -> "cache cold: next turn re-writes ~84k tokens"
func TestFlushNoStakesWhenUnknown(t *testing.T)      // no cache state -> the body equals Plan 4's golden
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/notify/ ./cmd/curia/ -run 'Stakes|ColdLine' -v`. Expected: FAIL, too many arguments in call to `DecisionPush`.
- [ ] **Step 3: Implement.** Every existing call and test of `DecisionPush` gains the fifth argument `""`.
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/notify/ ./cmd/curia/ ./e2e/ -v`. Expected: PASS, Plan 4's acceptance test unchanged.
- [ ] **Step 5: Commit.** `git commit -m "feat: cache stakes line in a blocking ticket's push"`

**Done when:** the five tests pass and Plan 4's push golden files are unchanged.

## 9. Tasks 2, 3, 6, 7, 8, 10 and 12

### Task 2: Usage: ingest contract, status line, cost-lens summary

**Model:** Sonnet (pure parsing with exact signatures and table tests).

**Needs:** tasks: 1. **Touches:** `internal/usage/ingest.go`, `ingest_test.go`, `costlens.go`, `costlens_test.go`, `testdata/`.

**Files:**

- Create: `internal/usage/ingest.go`, `internal/usage/costlens.go`, `internal/usage/testdata/` (`doc-spec.json`: the spec's example with `at`; `statusline-built.json`: section 4.3's fields written by hand; `summary-built.json`: section 4.6's shape with three branches, written by hand; beside setup's `statusline-captured.json` and `summary-captured.json`)
- Test: `internal/usage/ingest_test.go`, `internal/usage/costlens_test.go`

**Brief includes:** sections 4.2 (the table and the first two rules), 4.3 (the table and the status text), 4.6 ("Reading it"), and section 3's items 1, 8 and 10.

**Interfaces:**

- Consumes: `model.UsageWindow`, `WindowFiveHour`, `WindowSevenDay` (Task 1); `sanitize.Line`.
- Produces (all pure; `now` is passed in):

```go
package usage

const MaxDoc = 64 << 10 // bytes of an ingest or status-line document

// Limit is one window's reading.
type Limit struct {
    UsedPct  float64
    ResetsAt time.Time // zero: none, or out of range
}
// Doc is one accepted document. A Has field is false when the document did not carry that value or it was refused.
type Doc struct {
    SessionID       string
    At              time.Time // the reading's time; now when the document had none or a bad one
    CostUSD         float64
    HasCost         bool
    CacheExpiresAt  time.Time
    HasCacheExpiry  bool
    CacheRebuildUSD float64
    HasCacheRebuild bool
    Limits          map[model.UsageWindow]Limit // never nil
    Dropped         []string                    // names of fields that were present and refused, sorted
    ModelName       string                      // status line only; cleaned, at most 24 runes
    ContextPct      int                         // status line only
    HasContext      bool
}
// ErrDoc is wrapped when a whole document is refused; the error text names the reason and never a value.
var ErrDoc = errors.New("usage: document refused")

func ValidSessionID(id string) bool                             // ^[A-Za-z0-9_-]{1,64}$
func ParseDoc(raw []byte, now time.Time) (Doc, error)           // section 4.2
func FromStatusLine(raw []byte, now time.Time) (Doc, error)     // section 4.3
func StatusText(d Doc) string                                   // "<model> · ctx 71% · $4.12 · 5h 62%", at most 80 cells, "" when d has nothing to show
func BranchCost(summary []byte, branch string) (usd float64, ok bool) // section 4.6
```

Rules. A number is accepted only as a JSON number that is finite and in range: a string `"4.12"`, `NaN`, a boolean or an object in its place drops the field. A time is an RFC 3339 string, or for `resets_at` also a JSON number of Unix seconds. Field names in `Dropped` are the document's own dotted names (`cost_usd`, `rate_limits.five_hour.resets_at`). A `rate_limits` window whose `used_percentage` is missing or bad is dropped whole (`rate_limits.five_hour`). Unknown fields are ignored at every level. Duplicate keys: the last wins, as `encoding/json` does. `StatusText` shows the five-hour reading only, the cost with two decimals, and a context percentage from 0 to 100. `BranchCost` compares the branch name exactly and returns the sum of `input`, `output`, `cacheWrite` and `cacheRead`.

- [ ] **Step 1: Write the failing tests**

```go
// ingest_test.go
func TestParseDocSpecExample(t *testing.T)        // doc-spec.json -> every field of the spec's example: cost 4.12, cache expiry and rebuild 1.68, five_hour 62 and seven_day 31 with their resets, At from "at"
func TestParseDocMinimal(t *testing.T)            // {"schema_version":1,"session_id":"s1"} -> a Doc with no Has field set, Limits empty and non-nil, At == now
func TestParseDocRefusals(t *testing.T)
// "", "[]", "null", "{", 64 KiB + 1 byte, schema_version 2, "1", missing; session_id missing, "", "a b", "'; DROP TABLE sessions;--", 65 runes -> ErrDoc; the error text holds no part of the input
func TestParseDocDropsBadFieldsOnly(t *testing.T)
// cost_usd -1, 1e308, "4.12", true; cache_expires_at "soon", now+3h, now-8d; cache_rebuild_usd 1001; used_percentage 101, -1, "62";
// resets_at now+9d -> the field (or the window) is in Dropped, every other field of the same document is kept
func TestParseDocResetsAtForms(t *testing.T)      // an RFC 3339 string and the same instant as Unix seconds give equal times
func TestParseDocAtBounds(t *testing.T)           // at now-25h, now+2m, "x" -> At == now and "at" in Dropped; now-15m kept
func TestParseDocNullIsAbsent(t *testing.T)       // "cost_usd": null -> HasCost false, not in Dropped
func TestParseDocIgnoresUnknownAndOwnedFields(t *testing.T) // model, context_used, context_max, "extra": {...} -> no effect, not in Dropped
func TestFromStatusLineBuilt(t *testing.T)        // statusline-built.json -> session id, cost, both readings, model name, context 71
func TestFromStatusLineCaptured(t *testing.T)     // statusline-captured.json -> a Doc with a session id (t.Skip naming probe S1 while the file is absent)
func TestFromStatusLineWithoutLimitsOrCost(t *testing.T) // an API-key session's payload: no rate_limits -> Limits empty; no cost -> HasCost false
func TestFromStatusLineRefusals(t *testing.T)     // no session_id, 64 KiB + 1, an array -> ErrDoc
func TestStatusTextIsOneCleanLine(t *testing.T)
// a full Doc -> "Opus 5.5 · ctx 71% · $4.12 · 5h 62%"; only a model -> "Opus 5.5"; an empty Doc -> "";
// a model name "\x1b[31mX\nY" + 100 runes -> no control rune, no newline, the whole at most 80 cells
func TestSchemaFileMatchesParser(t *testing.T)    // ../../docs/ingest-schema.json: its "properties" keys are exactly the names ParseDoc reads or ignores by name; "required" is [schema_version, session_id]; its first example parses with nothing dropped
// costlens_test.go
func TestBranchCostBuilt(t *testing.T)            // summary-built.json, branch "curia/T-1-fix" with parts 0.5, 1.25, 2, 1.28 -> 5.03, true
func TestBranchCostCaptured(t *testing.T)         // summary-captured.json: the placeholder branch it names -> its four parts summed
func TestBranchCostRefusals(t *testing.T)
// a branch that is absent; currentProject null; "<html>"; an empty input; a cost part that is a string, negative, 1e12 or missing; a branches value that is an object -> 0, false
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/usage/ -v`. Expected: FAIL, the package does not exist.
- [ ] **Step 3: Implement.** Decode into `map[string]json.RawMessage` per level so that one bad field does not fail the document.
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/usage/ -v`. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat: ingest contract, status-line mapping and cost-lens summary reader"`

**Done when:** the seventeen tests pass.

### Task 3: Usage: forecast, burn, cells

**Model:** Opus (the edge cases are judgment: resets, sparse samples, merged sessions).

**Needs:** tasks: 1. **Touches:** `internal/usage/forecast.go`, `forecast_test.go`.

**Files:**

- Create: `internal/usage/forecast.go`
- Test: `internal/usage/forecast_test.go`

**Brief includes:** section 4.4, and section 3's item 2.

**Interfaces:**

- Consumes: `model.UsageSample`, `model.CostSample`, `model.UsageWindow`, `model.Session` with its cost and cache fields (Task 1); the state constants of `internal/model`.
- Produces (all pure):

```go
const (
    BurnWindow  = 20 * time.Minute
    MinBurnSpan = 3 * time.Minute
    APIHorizon  = 2 * time.Hour
)
// Forecast is one usage window as of its newest sample in the burn window.
type Forecast struct {
    Window   model.UsageWindow
    UsedPct  float64
    ResetsAt time.Time // zero: unknown
    PerHour  float64   // percentage points per hour; 0 when no burn could be measured
    LimitAt  time.Time // zero when PerHour is 0
    Warn     bool      // the limit lands before the reset
}
// Forecasts returns one Forecast per window that has a sample in [now-BurnWindow, now], five_hour first.
func Forecasts(samples []model.UsageSample, now time.Time) []Forecast
// Warning returns the warning forecast whose limit is soonest.
func Warning(f []Forecast, now time.Time) (Forecast, bool)
// BadgeText is "5h 62% → limit ~48m (resets 1h50m)" for a warning forecast.
func BadgeText(f Forecast, now time.Time) string
// Line is one line of curia usage for f: "5h 62% · resets 1h50m · burning 51%/h → limit ~48m", or "… · steady".
func Line(f Forecast, now time.Time) string
// Short renders a duration as "48m", "1h50m" or "3d4h"; never less than "1m".
func Short(d time.Duration) string
// Burns is each session's cost burn over the burn window, in USD per hour.
func Burns(cost []model.CostSample, now time.Time) map[string]float64
// APIProjection is the spend of the next APIHorizon at the current rate across the live sessions.
// ok is false when usage has a sample in the burn window (a subscription) or cost has none.
func APIProjection(usage []model.UsageSample, cost []model.CostSample, live map[string]bool, now time.Time) (usd float64, ok bool)
// Running is one running task, for SuggestPause.
type Running struct {
    TaskID   string
    N        int64
    Priority int     // 1 to 3; 0 counts as 2
    BurnUSD  float64 // per hour, summed over its sessions
}
// SuggestPause picks the task to pause: the lowest priority, then the highest burn, then the newest.
func SuggestPause(r []Running) (taskID string, ok bool)
func CostCell(usd float64, known bool) string
func CacheCell(s model.Session, now time.Time) string
```

Rules: section 4.4, exactly. `Forecasts` does not change its input and does not assume it is sorted. `cost` given to `Burns` and `APIProjection` may hold, per session, one sample older than the window (the baseline `store.CostSince` adds); a session with only one sample in total burns 0.

- [ ] **Step 1: Write the failing tests**

```go
func TestForecastSpecExample(t *testing.T)
// five_hour samples at now-15m 50%, now-8m 56%, now-1m 62%, resets now+1h50m -> UsedPct 62, PerHour ≈ 51.4, LimitAt ≈ now+43m20s, Warn true;
// BadgeText == "5h 62% → limit ~43m (resets 1h50m)"
func TestForecastNoWarnWhenResetComesFirst(t *testing.T)  // the same burn, resets now+30m -> Warn false; Warning reports none
func TestForecastNeedsSpan(t *testing.T)                  // one sample; two samples 2m59s apart; three samples all older than 20m -> PerHour 0, Warn false
func TestForecastSteadyIsNoWarning(t *testing.T)          // 62, 62, 62 over 15 minutes -> PerHour 0; Line ends "· steady"
func TestForecastCutsAtReset(t *testing.T)                // 90, 95, then 2, 4, 6 -> the series is 2, 4, 6; UsedPct 6
func TestForecastMergesSessions(t *testing.T)             // the samples above spread over three session ids, unsorted -> the same Forecast
func TestForecastWithoutResetTimeNeverWarns(t *testing.T) // a rising series whose last sample has no ResetsAt -> PerHour set, Warn false
func TestForecastAtLimit(t *testing.T)                    // last sample 100% with the reset ahead -> Warn true; BadgeText == "5h 100% → at limit (resets 1h50m)"
func TestForecastPastLimitAt(t *testing.T)                // LimitAt already before now -> "at limit", never a negative duration
func TestWarningPicksSoonest(t *testing.T)                // five_hour limit in 48m and seven_day in 30m, both warning -> seven_day; BadgeText starts "7d "
func TestForecastsOrderAndAbsence(t *testing.T)           // only seven_day has samples -> one Forecast; none -> empty, not nil-panicking
func TestShort(t *testing.T)                              // 20s -> "1m"; 48m10s -> "48m"; 110m -> "1h50m"; 2h -> "2h0m"; 76h -> "3d4h"; a negative -> "1m"
func TestBurns(t *testing.T)                              // s1: baseline 3.00 at now-25m, 4.00 at now-2m -> 3.00 per hour; s2: one sample -> 0; s3: cost fell (4 then 1) -> 0
func TestAPIProjection(t *testing.T)                      // no usage sample in the window; s1 +1.00 and s2 +0.50 in it, both live -> 9.00, true; s2 not live -> 6.00
func TestAPIProjectionOffOnSubscription(t *testing.T)     // one usage sample in the window -> ok false; no cost sample -> ok false
func TestSuggestPause(t *testing.T)                       // P2 burning 5 and P3 burning 1 -> the P3; two P3 -> the higher burn; equal burn -> the higher N; priority 0 ranks as P2; empty -> ok false
func TestCostCell(t *testing.T)                           // 4.123 -> "$4.12"; 0.07 -> "$0.07"; 123.4 -> "$123"; unknown -> ""
func TestCacheCell(t *testing.T)
// idle, expiry now+12m30s -> "12m"; now+20s -> "1m"; expired with rebuild 1.68 -> "cold $1.68"; expired without -> "cold"; unknown -> "";
// a working session with a warm cache -> ""; waiting and needs_decision behave as idle
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/usage/ -run 'Forecast|Warning|Short|Burns|API|Suggest|Cell' -v`. Expected: FAIL, undefined `Forecasts`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/usage/ -v`. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat: usage forecast, burn attribution and the cost and cache cells"`

**Done when:** the eighteen tests pass and no function in the file reads a clock.

### Task 6: Store: ingest and samples

**Model:** Opus (a write transaction that runs beside concurrent hooks).

**Needs:** tasks: 1. **Touches:** `internal/store/migrations/0006_cost.sql`, `usage.go`, `usage_test.go`, `store.go`, `store_test.go`.

**Files:**

- Create: `internal/store/migrations/0006_cost.sql` (section 4.1, exactly), `internal/store/usage.go`
- Modify: `internal/store/store.go` (`selectSessions`, `scanSession`), `internal/store/store_test.go` (migration count 6)
- Test: `internal/store/usage_test.go`

**Brief includes:** sections 4.1 and 4.2 (the `store.Ingest` rule), and section 3's items 3, 4 and 11.

**Interfaces:**

- Consumes: `model.UsageSample`, `CostSample`, the new `model.Session` fields (Task 1); `begin`, `formatTime`, `parseTime`, `upsert`, `selectSessions`, `scanSession` (`internal/store`, E2).
- Produces:

```go
const (
    SampleRepeat = 60 * time.Second    // an identical reading inside this adds no row
    UsageKeep    = 8 * 24 * time.Hour
    CostKeep     = 24 * time.Hour
    PruneBatch   = 200
)
// Reading is what one ingest stores. A nil pointer or a zero time means "not carried": that value is left as it is.
type Reading struct {
    SessionID       string
    At              time.Time
    CostUSD         *float64
    CacheExpiresAt  time.Time
    CacheRebuildUSD *float64
    Limits          []model.UsageSample // Window, UsedPct and ResetsAt set; At and SessionID are filled from the Reading
}
// IngestResult says what an ingest changed.
type IngestResult struct {
    SessionFound          bool
    UsageAdded, CostAdded int
    Pruned                int
}
// Ingest stores r in one write transaction (plan section 4.2).
func (s *Store) Ingest(ctx context.Context, r Reading, now time.Time) (IngestResult, error)
// UsageSince returns the usage samples taken at or after since, by at then n.
func (s *Store) UsageSince(ctx context.Context, since time.Time) ([]model.UsageSample, error)
// CostSince returns the cost samples taken at or after since and, per session, its newest sample before since, by session, at, n.
func (s *Store) CostSince(ctx context.Context, since time.Time) ([]model.CostSample, error)
```

Rules: section 4.2's four steps, in one `BEGIN IMMEDIATE` transaction. `selectSessions` reads `cost_usd` (a NULL gives `CostKnown` false), `cache_expires_at` and `COALESCE(cache_rebuild_usd, 0)`; `upsert` is not changed and still writes none of them. The store takes a `Reading`, not a `usage.Doc`, so it does not depend on the parser; Task 13 converts. Pruning deletes by `n` with a `LIMIT`, oldest first, usage samples first, at most `PruneBatch` rows in total.

- [ ] **Step 1: Write the failing tests**

```go
func TestMigrationAddsCostTables(t *testing.T)          // usage_samples, cost_samples and their indexes exist; tasks has priority and agent; decisions has exported_at; decisions_fts passes integrity-check
func TestMigrationOnPlan4Database(t *testing.T)         // a database built by migrations 0001 to 0005 with a session, a task and a decision -> migrates; the task reads priority 2 and agent "claude"; nothing else changed
func TestMigrationChecks(t *testing.T)                  // raw inserts: window 'one_hour', used_pct 101, cost_usd -1, priority 4, agent 'gemini' -> each fails
func TestSessionCostAndCacheRoundTrip(t *testing.T)     // after an Ingest, GetSession and ListSessions return CostUSD, CostKnown, CacheExpiresAt, CacheRebuildUSD; a session never ingested has CostKnown false and a zero expiry
func TestOldUpsertKeepsCostColumns(t *testing.T)        // Ingest, then UpsertSession and UpdateSession on the same row -> the three columns are unchanged
func TestIngestWritesSessionAndSamples(t *testing.T)    // a full Reading for a known session -> the three columns, one cost sample, two usage samples carrying the Reading's At and session id; result {true, 2, 1, 0}
func TestIngestAbsentFieldsChangeNothing(t *testing.T)  // a full Reading, then one with only a cost -> cache expiry and rebuild keep the first values; then one with nothing -> no change, no sample
func TestIngestIsNotActivity(t *testing.T)              // updated_at and seen_at are byte-equal before and after
func TestIngestUnknownSessionKeepsUsageOnly(t *testing.T) // no row is created; no cost sample; the usage samples are stored; SessionFound false
func TestIngestSkipsRepeatedReading(t *testing.T)       // the same reading 30 s later -> no new row; 61 s later -> a new row; a changed used_pct 5 s later -> a new row; the same for cost
func TestIngestPrunesBounded(t *testing.T)              // 500 usage samples 9 days old and 300 cost samples 2 days old -> one Ingest deletes exactly 200, the oldest; fresh rows are untouched
func TestIngestSurvivesConcurrentHook(t *testing.T)     // 8 goroutines: half UpdateSession the row (state and last update), half Ingest rising costs, 50 rounds, -race -> no error, the final state is the last hook's and the final cost the highest
func TestConcurrentIngestsMergeIntoOneSeries(t *testing.T) // three sessions ingesting the same window at once -> UsageSince returns them all, ordered by at then n
func TestUsageSinceOrderAndBound(t *testing.T)
func TestCostSinceIncludesBaseline(t *testing.T)        // s1 has samples at now-40m, now-25m, now-5m; since now-20m -> the now-25m and now-5m samples of s1, not the now-40m one
func TestTicketsSeeIngestedCache(t *testing.T)          // two open blocking tickets of two sessions; Ingest gives the newer ticket's session a cache expiring in 5 minutes -> OpenTickets carries it and decision.Triage puts that ticket first (F17 with real values)
func TestIngestOnClosedStoreIsAnError(t *testing.T)     // returns an error, does not panic
```

  `TestOpenIsIdempotent` and `TestConcurrentOpenOfFreshFile` expect 6 migrations.

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/store/ -run 'Cost|Ingest|Usage|Migration|TicketsSee' -v`. Expected: FAIL, undefined `Ingest`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/store/ -v`. Expected: PASS, every earlier store test included.
- [ ] **Step 5: Commit.** `git commit -m "feat: ingest transaction, usage and cost samples, migration 0006"`

**Done when:** the seventeen tests pass under `-race`.

### Task 7: Store: priority, agent, export mark

**Model:** Sonnet (three columns through existing statements, exact tests).

**Needs:** tasks: 6. **Touches:** `internal/store/tasks.go`, `tasks_test.go`, `decisions.go`, `decisions_test.go`.

**Files:**

- Modify: `internal/store/tasks.go`, `internal/store/decisions.go`
- Test: `internal/store/tasks_test.go`, `internal/store/decisions_test.go`

**Brief includes:** section 4.1 (the three `ALTER` lines) and section 4.7's "When" item.

**Interfaces:**

- Consumes: `model.Task.Priority`, `Task.Agent`, `model.Decision.ExportedAt` (Task 1); the columns of migration 0006 (Task 6); the task statements of `tasks.go` (E4); `selectDecisions`, `scanDecision` (`decisions.go`).
- Produces:
  - Every task read (`GetTask`, `ListTasks`, the row `UpdateTask` loads, what `CreateTask` and `ImportTask` return) carries `Priority` and `Agent`. `CreateTask` and `ImportTask` (when it creates) store them, a zero priority as 2 and an empty agent as `claude`. `UpdateTask` writes both. `ImportTask` updating an existing row still writes title, body and tags only.
  - Every decision read carries `ExportedAt`.

```go
// ExportQueue returns decided decisions whose note is due: never exported, or exported before their answer or supersession.
// With all set it returns every decided decision. By n, at most limit.
func (s *Store) ExportQueue(ctx context.Context, all bool, limit int) ([]model.Decision, error)
// MarkExported records that the decision's note was written at at. ErrNotFound for an unknown id.
func (s *Store) MarkExported(ctx context.Context, id string, at time.Time) error
```

Rules. "Decided" is status `answered`, `confirmed` or `overridden`. `MarkExported` is one `UPDATE` that touches only `exported_at`, so it does not disturb `decisions_fts`. Timestamps compare as the store's fixed-width text.

- [ ] **Step 1: Write the failing tests**

```go
// tasks_test.go
func TestTaskPriorityAndAgentRoundTrip(t *testing.T)   // create with priority 3, agent "codex" -> GetTask and ListTasks return them; create with zero values -> 2 and "claude"
func TestUpdateTaskWritesPriorityAndAgent(t *testing.T)
func TestImportKeepsMyPriority(t *testing.T)           // import an issue with priority 1; UpdateTask sets 3; import the same source ref again with priority 1 -> still 3, the title updated
func TestOldTaskUpdateKeepsPriority(t *testing.T)      // a raw UPDATE naming only the columns of migrations 0004 and 0005 (what an older binary runs) -> priority and agent unchanged
func TestStoreRejectsBadPriorityAndAgent(t *testing.T) // priority 4, agent "gemini" through CreateTask and UpdateTask -> an error, no row changed
// decisions_test.go
func TestExportQueueDue(t *testing.T)
// open, withdrawn, expired -> never; answered and never exported -> due; marked exported after the answer -> not due;
// then superseded -> due again; a standing rule -> due; all=true -> every decided one; limit 2 of 5 -> the two lowest n
func TestMarkExported(t *testing.T)                    // sets exported_at; GetDecision returns it; unknown id -> ErrNotFound; decisions_fts integrity-check passes
func TestMarkExportedRacesAnswer(t *testing.T)         // MarkExported and EditDecision (supersede) on one row from two goroutines, -race -> both applied, no error
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/store/ -run 'Priority|Agent|Export' -v`. Expected: FAIL, undefined `ExportQueue`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/store/ -v`. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat: store task priority and agent, and the export mark of a decision"`

**Done when:** the eight tests pass and no earlier store test changed its assertions.

### Task 8: Hook and state: the session's agent

**Model:** Opus (the hook path and its budget).

**Needs:** tasks: 1. **Touches:** `internal/state/state.go`, `state_test.go`, `internal/hook/run.go`, `hook_test.go`, `internal/hook/testdata/codex/`, `cmd/curia/hook.go`, `hook_test.go`.

**Files:**

- Create: `internal/hook/testdata/codex/` (`built-session-start.json`, `built-pre-tool-use.json`, `built-stop.json`: Claude Code's payload fields with a Codex-style transcript path, written by hand; setup's captured files of probe X1 beside them when they exist)
- Modify: `internal/state/state.go`, `internal/hook/run.go`, `cmd/curia/hook.go`
- Test: `internal/state/state_test.go`, `internal/hook/hook_test.go`, `cmd/curia/hook_test.go`

**Brief includes:** section 4.10 ("Hook" and "Delivery"), section 2's hook budget and output, and section 3's item 9.

**Interfaces:**

- Consumes: `model.Event.Agent`, `model.AgentClaude`, `model.AgentCodex` (Task 1); `hook.Env`, `hook.Run`, `runStop`, `readTranscript` (E15); `state.Apply`.
- Produces:
  - `hook.Env` gains `Agent string`: "the agent named on the command line; empty means Claude Code". `hook.Run` sets `ev.Agent` from it after parsing.
  - `state.Apply`: where a session is started, `s.Agent` is `ev.Agent`, or `model.AgentClaude` when that is empty. An existing session's agent never changes.
  - In `internal/hook/run.go`: when `ev.Agent == model.AgentCodex`, `readTranscript` is not called, and a Stop runs the session update without `ClaimDeliveries` and without the message claim, so `Result` has no output and the session becomes `idle`, or `needs_decision` with an open blocking ticket, as for any Stop that claimed nothing.
  - `cmd/curia/hook.go`: `hookArgs(args []string) (event, agent string)` reads `<Event>` and an optional `--agent <name>` or `--agent=<name>` after it; `codex` is the only accepted name, any other is logged at WARN and treated as empty; extra arguments are logged and ignored, as today.

- [ ] **Step 1: Write the failing tests**

```go
// internal/state/state_test.go
func TestStartTakesAgentFromEvent(t *testing.T)        // a SessionStart with Agent "codex" -> Session.Agent "codex"; with "" -> "claude"
func TestAgentNeverChangesAfterStart(t *testing.T)     // a claude session receiving an event with Agent "codex" keeps "claude"
// internal/hook/hook_test.go
func TestCodexSessionIsRecorded(t *testing.T)          // Env.Agent "codex", built-session-start.json -> a row with agent codex, the pane, cwd, repo and branch
func TestCodexTranscriptNotRead(t *testing.T)          // the payload's transcript_path names a file of Claude Code transcript lines -> model empty, context 0, transcript offset 0
func TestCodexStopClaimsNothing(t *testing.T)          // an answer and a message queued for the session; Stop with Env.Agent "codex" -> Result has no output; both stay queued; the state is idle
func TestCodexStopWithOpenBlockingTicket(t *testing.T) // -> needs_decision
func TestClaudeStopUnchanged(t *testing.T)             // the same queue with Env.Agent "" -> the block line, as before this task
func TestCodexHookAddsNoCall(t *testing.T)             // a counting Env: SessionStart makes one PaneInfo and one Git call, as for Claude Code; the next five events none
// cmd/curia/hook_test.go
func TestHookArgs(t *testing.T)                        // [Stop] -> Stop, ""; [Stop --agent codex] and [Stop --agent=codex] -> codex; [Stop --agent gemini] -> "" and a WARN line; [--agent codex] -> event "--agent", refused as an unknown event
func TestHookCodexPrintsNothingAtStop(t *testing.T)    // the binary's command path with a queued answer: stdout and stderr are empty, exit 0
func TestHookLatencyUnchangedWithAgentFlag(t *testing.T) // TestHookLatency's method and threshold (p95 < 20 ms, 200 samples, no retry) with --agent codex; p50 and p95 reported
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/state/ ./internal/hook/ ./cmd/curia/ -run 'Agent|Codex|HookArgs' -v`. Expected: FAIL, `Env` has no field `Agent`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/state/ ./internal/hook/ ./cmd/curia/ -v`. Expected: PASS, every earlier hook and latency test unchanged.
- [ ] **Step 5: Commit.** `git commit -m "feat: record a session's agent; Codex sessions through the same hook"`

**Done when:** the eleven tests pass, the hook's stdout and exit rules are untouched for Claude Code (the existing `TestStdoutOnlyOnBlockedStop…` tests pass unchanged), and the PR states p50 and p95. If probe X1's captured payloads exist, `TestCodexSessionIsRecorded` also runs over each of them.

### Task 10: Install: Codex

**Model:** Opus (another program's configuration, unverified until its probes).

**Needs:** tasks: 9 (the shared `cmd/curia/install.go`); probes X1 and X2 recorded. **Touches:** `internal/install/codex.go`, `codex_test.go`, `testdata/codex/`, `cmd/curia/install.go`, `install_test.go`.

**Held:** this task is dispatched only when setup has committed `internal/install/testdata/codex/` (`hooks-captured.json`: a hooks file Codex accepted; `config-with-curia.toml`: `config.toml` after `codex mcp add curia -- /x/curia mcp`; `config-user.toml`: one with the user's own servers). Nothing else waits for it: no task needs Task 10, and Task 22 runs its Codex install step only when this task has merged.

**Files:**

- Create: `internal/install/codex.go`
- Modify: `cmd/curia/install.go`
- Test: `internal/install/codex_test.go`, `cmd/curia/install_test.go`

**Brief includes:** section 4.10 ("Install"), probe X1's and X2's reports, section 2's "Installer rulings", and section 3's item 7.

**Interfaces:**

- Consumes: `hookEvents`, `ourEntry`'s shape, `withoutOurs`, `containsOurs`, `writeBackup`, `writeAtomic`, `shellQuote` (`internal/install/settings.go`); `install.Runner`, `MCPState` and its values (`internal/install/mcp.go`); `executablePath`, `installSay` (E14).
- Produces:

```go
// CodexDir is codexHome ($CODEX_HOME) when set, else <home>/.codex.
func CodexDir(home, codexHome string) string
// CodexHookCommand is the command of Curia's Codex hook for one event: "<quoted bin> hook <Event> --agent codex".
func CodexHookCommand(bin, event string) string
// CodexHooks adds Curia's entry for each event Codex supports to hooksPath, keeping every other entry, backup first.
func CodexHooks(hooksPath, bin string, now time.Time) error
// CodexUnhooks removes exactly the entries CodexHooks added for bin.
func CodexUnhooks(hooksPath, bin string) error
// AddCodexMCP registers "curia" with `codex mcp add curia -- <bin> mcp` unless configPath already holds it.
func AddCodexMCP(ctx context.Context, run Runner, configPath, bin string) (MCPState, error)
// RemoveCodexMCP runs `codex mcp remove curia` when configPath holds Curia's entry for bin.
func RemoveCodexMCP(ctx context.Context, run Runner, configPath, bin string) (MCPState, error)
```

  - `cmd/curia/install.go`: `var codexRunner install.Runner` (runs `codex` found by `exec.LookPath`, 30 second limit); `curia install --codex` runs the two steps after the Claude Code steps and prints one line each (`codex hooks: added`, `codex MCP server: registered`, `… already registered`, `… a server named curia that is not Curia's is registered; left as it is`, `codex is not on PATH; nothing done for Codex`); `curia uninstall` runs both removals when the Codex directory exists. Without `--codex`, install never reads or writes the Codex directory.

Rules. The hooks file's path, its top-level shape and the event names are those of probe X1's report, pasted into the brief; an entry is Curia's when its command equals `CodexHookCommand(bin, event)`. The MCP entry is recognized by reading `config.toml` with the pinned TOML library, read-only: the table probe X2 recorded (assumed `[mcp_servers.curia]` with `command` and `args`); Curia never writes `config.toml` itself and never runs `codex mcp list` or `get`. `auth.json` is never opened. A missing Codex directory with `--codex` is `codex has no configuration directory yet: start codex once, then run curia install --codex again`.

- [ ] **Step 1: Write the failing tests**

```go
// internal/install/codex_test.go
func TestCodexDir(t *testing.T)                        // CODEX_HOME set, unset, relative (used as given, as CLAUDE_CONFIG_DIR is)
func TestCodexHooksAddsToCapturedFile(t *testing.T)    // hooks-captured.json -> one Curia entry per event, each command as CodexHookCommand; the captured entries byte-equal in meaning; one backup
func TestCodexHooksKeepUserEntries(t *testing.T)       // a file with the user's own hook on Stop -> still there, first, after add and after remove
func TestCodexHooksIdempotent(t *testing.T)            // a second call changes nothing and writes no backup
func TestCodexHooksCreatesMissingFile(t *testing.T)
func TestCodexUnhooksOnlyOurs(t *testing.T)            // entries of another curia path and the user's stay
func TestCodexHooksBadShapeIsAnError(t *testing.T)     // a hooks file that is an array or holds a string where a list belongs -> an error naming the file; nothing written
func TestAddCodexMCPSequence(t *testing.T)             // config-user.toml -> the runner got exactly ["mcp","add","curia","--","<bin>","mcp"]; state added
func TestAddCodexMCPAlreadyOurs(t *testing.T)          // config-with-curia.toml for the same bin -> no run; state already
func TestAddCodexMCPForeign(t *testing.T)              // a curia table with another command -> no run; state foreign
func TestRemoveCodexMCPOnlyOurs(t *testing.T)
func TestCodexNeverReadsAuthOrWritesConfig(t *testing.T) // config.toml is mode 0400 and byte-equal afterwards; auth.json is a directory in the test, so any attempt to read it as a file fails the run
// cmd/curia/install_test.go
func TestInstallCodexOnlyWithFlag(t *testing.T)        // without --codex the stub runner is never called and the Codex directory is byte-equal
func TestInstallCodexThroughStub(t *testing.T)         // with --codex and a stub runner -> both lines printed, the hooks file written
func TestInstallCodexNotOnPath(t *testing.T)
func TestUninstallReversesCodex(t *testing.T)
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/install/ ./cmd/curia/ -run Codex -v`. Expected: FAIL, undefined `CodexHooks`.
- [ ] **Step 3: Implement.** No test runs the real `codex`.
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/install/ ./cmd/curia/ -v`. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat: curia install --codex registers hooks and the MCP server with Codex"`

**Done when:** the sixteen tests pass.

### Task 12: Fleet: forecast in the badge, cost and cache of a unit

**Model:** Sonnet (pure, two small additions).

**Needs:** tasks: 1. **Touches:** `internal/fleet/fleet.go`, `fleet_test.go`, `units.go`, `units_test.go`.

**Files:**

- Modify: `internal/fleet/fleet.go`, `internal/fleet/units.go`
- Test: `internal/fleet/fleet_test.go`, `internal/fleet/units_test.go`

**Brief includes:** section 4.5's "Badge" item and the team sentence of its "Dashboard" item.

**Interfaces:**

- Consumes: `fleet.Counts`, `fleet.Badge`, `fleet.Unit` (E9); the cost and cache fields of `model.Session` (Task 1).
- Produces:
  - `fleet.Counts` gains `Forecast string`: "the usage warning, already rendered; empty when there is none". `fleet.Badge` joins, with single spaces: the counts that are not zero, then `Forecast` when not empty, then `auto-off` when set. `fleet.Count` keeps its signature.

```go
// Cost is the sum of the unit's members' known costs; known is false when no member has one.
func (u Unit) Cost() (usd float64, known bool)
// CacheOf returns the member whose prompt cache stands for the unit at now: among members that are idle, waiting or
// needs_decision and have a known cache state, the one whose warm cache expires soonest, else one whose cache is cold.
func (u Unit) CacheOf(now time.Time) (model.Session, bool)
```

- [ ] **Step 1: Write the failing tests**

```go
// fleet_test.go
func TestBadgeWithForecast(t *testing.T)
// counts D2 W1 and Forecast "5h 62% → limit ~48m (resets 1h50m)" -> "D2 W1 5h 62% → limit ~48m (resets 1h50m)";
// no counts -> the forecast alone; with AutoOff -> "… (resets 1h50m) auto-off"; no forecast -> the badge of before, byte for byte
// units_test.go
func TestUnitCost(t *testing.T)            // lead 1.50 known, teammate 2.00 known, teammate unknown -> 3.50, true; none known -> 0, false; a solo session -> its own
func TestUnitCacheOf(t *testing.T)
// lead idle warm 12m, teammate waiting warm 4m, teammate working warm 1m -> the 4m teammate;
// only cold members -> a cold one; a team whose idle members are all unknown -> false; a solo working session -> false
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/fleet/ -run 'Forecast|UnitCost|UnitCache' -v`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/fleet/ -v`. Expected: PASS, every existing badge test unchanged.
- [ ] **Step 5: Commit.** `git commit -m "feat: usage forecast in the badge; cost and cache of a team unit"`

**Done when:** the three tests pass.

## 10. Tasks 13 to 21

### Task 13: `curia ingest`, `curia statusline`

**Model:** Opus (a process boundary with a never-fail rule and a latency budget).

**Needs:** tasks: 2, 6. **Touches:** `cmd/curia/ingest.go`, `ingest_test.go`, `statusline.go`, `statusline_test.go`.

**Files:**

- Create: `cmd/curia/ingest.go`, `cmd/curia/statusline.go`
- Test: `cmd/curia/ingest_test.go`, `cmd/curia/statusline_test.go`

**Brief includes:** sections 4.2 (the last two rules) and 4.3 (the `curia statusline` paragraph), section 2's "never fail their caller", and section 3's items 1 and 8.

**Interfaces:**

- Consumes: `usage.ParseDoc`, `FromStatusLine`, `StatusText`, `MaxDoc`, `Doc`, `ErrDoc` (Task 2); `store.Ingest`, `store.Reading` (Task 6); `loadConfig`, `openStore`, `openLog`, `clock` (`cmd/curia/common.go`); the hook's way of ignoring `SIGPIPE` and its watchdog pattern (`cmd/curia/hook.go`).
- Produces, in package `main`:

```go
// ingest.go
var ingestLimit = 2 * time.Second // the whole run; tests shorten it
// ingestReading converts an accepted document into what the store takes.
func ingestReading(d usage.Doc) store.Reading
// ingestRun reads one document from stdin (statusLine: Claude Code's status-line JSON), stores it and returns it.
// It never panics outward: a panic is recovered and returned as an error.
func ingestRun(ctx context.Context, stdin io.Reader, statusLine bool) (usage.Doc, error)
```

  - `curia ingest [--statusline]`: `Run` (not `RunE`) calls `ingestRun` under `ingestLimit`, logs an error at WARN (the reason and `Doc.Dropped`'s names, never a value or the document), and returns; the process exits 0 and prints nothing on every path. Dropped fields of an accepted document are logged at WARN too. Unknown flags and arguments are logged, not printed (`DisableFlagParsing`, as `hook` does, with `--statusline` read by hand).
  - `curia statusline`: the same run with `statusLine` true, then `fmt.Fprintln(stdout, usage.StatusText(doc))`; after any error it prints the text of whatever was parsed, or an empty line. Nothing on stderr. Exit 0.
  - Both read at most `usage.MaxDoc + 1` bytes of stdin and stop there.

- [ ] **Step 1: Write the failing tests**

```go
// ingest_test.go
func TestIngestStoresDocument(t *testing.T)            // a seeded session; the spec's example on stdin -> the row has cost 4.12, the cache expiry and rebuild; two usage samples and one cost sample exist
func TestIngestNeverFails(t *testing.T)
// stdin: empty, "garbage", 1 MiB of "a", "[]", schema_version 2; CURIA_DB pointing into a directory that does not exist; a config file that does not parse ->
// the command returns without error, stdout and stderr are empty, and the store (where reachable) is unchanged
func TestIngestPrintsNothing(t *testing.T)             // a good document and a bad one, through rootCmd with SetOut and SetErr -> both buffers empty
func TestIngestLogsNamesNotValues(t *testing.T)        // cost_usd -1 and session id "s1" -> the log line holds "cost_usd" and not "-1"; a refused document's log line holds no byte of the document
func TestIngestStatuslineFlag(t *testing.T)            // --statusline with statusline-built.json -> the samples and the cost are stored
func TestIngestAllowedInsideAgent(t *testing.T)        // CLAUDECODE=1 -> stored as usual
func TestIngestStdinDeadline(t *testing.T)             // a stdin that never closes, ingestLimit 100 ms -> returns within 1 s, nothing stored, nothing printed
func TestIngestRecoversPanic(t *testing.T)             // a stdin reader that panics -> no panic escapes, exit path normal
func TestIngestLatency(t *testing.T)                   // TestHookLatency's method: 200 sequential in-process runs on a database with 50 sessions and 5000 samples, p95 < 20 ms, no retry; p50 and p95 reported
// statusline_test.go
func TestStatuslinePrintsText(t *testing.T)            // statusline-built.json -> stdout is exactly "Opus 5.5 · ctx 71% · $4.12 · 5h 62%\n"; the samples are stored
func TestStatuslineAlwaysPrintsOneLine(t *testing.T)   // empty stdin, garbage, an unreachable store, a model name with escape sequences -> stdout is exactly one line with no control rune; stderr empty
func TestStatuslineUnreachableStoreStillShowsText(t *testing.T) // the store cannot be opened -> the text of the parsed document is still printed
func TestStatuslineSurvivesClosedStdout(t *testing.T)  // the built binary with stdout closed -> exit 0 (SIGPIPE ignored)
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./cmd/curia/ -run 'Ingest|Statusline' -v`. Expected: FAIL, undefined `ingestRun`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./cmd/curia/ -v`. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat: curia ingest and curia statusline"`

**Done when:** the thirteen tests pass and the PR states the latency figures.

### Task 14: `curia status` forecast, `curia usage`

**Model:** Sonnet (thin commands over finished rules and reads).

**Needs:** tasks: 3, 6, 12. **Touches:** `cmd/curia/status.go`, `status_test.go`, `usage.go`, `usage_test.go`.

**Files:**

- Create: `cmd/curia/usage.go`
- Modify: `cmd/curia/status.go`
- Test: `cmd/curia/status_test.go`, `cmd/curia/usage_test.go`

**Brief includes:** section 4.5's "Badge" and "`curia usage`" items, and section 4.4's "API math", "Attribution" and "Pause suggestion".

**Interfaces:**

- Consumes: `usage.Forecasts`, `Warning`, `BadgeText`, `Line`, `Burns`, `APIProjection`, `SuggestPause`, `Running`, `CostCell`, `BurnWindow` (Task 3); `store.UsageSince`, `CostSince` (Task 6); `fleet.Counts.Forecast` (Task 12); `statusBadge`, `listSettled`, `clock` (`cmd/curia/status.go`, `common.go`); `taskCards` (E6); `ui.Cell`, `sanitize.Line`.
- Produces, in package `main`:

```go
// usage.go
// usageReport is everything curia usage shows, computed once.
type usageReport struct {
    Forecasts  []usage.Forecast
    Warning    usage.Forecast
    Warn       bool
    APIUSD     float64
    API        bool
    Burn       []usageBurnRow // highest first
    PauseTask  string         // "" when there is no suggestion
    PauseLabel string         // "T-7 (P3) api-server"
    PauseTitle string
    Any        bool           // something was ingested in the last 24 hours
}
type usageBurnRow struct {
    SessionID, Window, TaskID string
    USDPerHour, CostUSD       float64
    CostKnown                 bool
}
// usageBuild reads the samples, the sessions and the task cards and applies section 4.4. It writes nothing.
func usageBuild(ctx context.Context, st *store.Store, now time.Time) (usageReport, error)
```

  - `curia usage [--json]` prints `usageBuild`'s report as section 4.5 says. Tasks 20 and 21 reuse `usageBuild`.
  - `statusBadge`: after the counts, `st.UsageSince(ctx, now.Add(-usage.BurnWindow))`, then `usage.Warning(usage.Forecasts(…), now)`; a warning sets `Counts.Forecast` to `usage.BadgeText`. A failed read leaves the forecast empty and the counts as they are (the badge still prints). `status` does not call `usageBuild`: one indexed read is all it adds.

Rules. A task's burn is the sum over its live linked sessions. `PauseLabel` is `<id> (P<n>) <repo base>` through `sanitize.Line(…, 60)`. Every printed cell goes through `ui.Cell`. The suggestion exists only while `Warn` is true.

- [ ] **Step 1: Write the failing tests**

```go
// status_test.go
func TestStatusShowsForecastWarning(t *testing.T)   // the three samples of TestForecastSpecExample through store.Ingest, one waiting session -> "W1 5h 62% → limit ~43m (resets 1h50m)"
func TestStatusNoForecastWithoutWarning(t *testing.T) // a steady series, a series resetting first -> the badge of before
func TestStatusForecastAloneWhenNoCounts(t *testing.T)
func TestStatusStillSilentOnUsageError(t *testing.T) // the usage_samples table dropped by raw SQL -> the counts still print, no error text
func TestStatusCostOfTheExtraRead(t *testing.T)     // 3000 tickets and 20000 samples: p50 and p95 of statusBadge reported; p95 < 20 ms
// usage_test.go
func TestUsageSubscription(t *testing.T)            // the warning series; two running tasks (P2 burning 5/h, P3 burning 1/h) -> the window line, the burn table in order, "suggestion: pause T-2 (P3) api: curia pause T-2"
func TestUsageNoSuggestionWithoutWarning(t *testing.T)
func TestUsageAPIMode(t *testing.T)                 // no usage samples; cost samples rising -> "next 2h ≈ $9.00 at the current rate"; no window line
func TestUsageNothingIngested(t *testing.T)         // -> the one line of section 4.5, exit 0
func TestUsageJSON(t *testing.T)                    // the keys of section 4.5, times RFC 3339 or null; an empty report is {"windows":[],"api_projection_usd":null,"burn":[],"suggest_pause":null}
func TestUsageTableIsSanitized(t *testing.T)        // a session whose window name and a task whose repo hold escape sequences -> none reaches stdout
func TestUsageAllowedInsideAgent(t *testing.T)      // CLAUDECODE=1 -> prints (it only reads)
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./cmd/curia/ -run 'Status|Usage' -v`. Expected: FAIL, undefined `usageBuild`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./cmd/curia/ -v`. Expected: PASS, every earlier status test unchanged.
- [ ] **Step 5: Commit.** `git commit -m "feat: limit forecast in the badge and curia usage"`

**Done when:** the twelve tests pass and the PR states `status`'s cost before and after.

### Task 15: Task priority

**Model:** Sonnet (a field through rules and three subcommands).

**Needs:** tasks: 7. **Touches:** `internal/task/task.go`, `task_test.go`, `cmd/curia/task.go`, `task_test.go`, `cmd/curia/testdata/tasks.golden.json`.

**Files:**

- Modify: `internal/task/task.go`, `cmd/curia/task.go`, `cmd/curia/testdata/tasks.golden.json` (by hand: `priority` and `agent` on every object)
- Test: `internal/task/task_test.go`, `cmd/curia/task_test.go`

**Brief includes:** section 4.5's "`curia task`" item.

**Interfaces:**

- Consumes: `task.Normalize`, `Validate`, `Problem` (E5); the store's priority and agent (Task 7); `curia task add`, `edit`, `ls` (E6).
- Produces:

```go
// ParsePriority reads "P1", "p2", "3": the priority 1 to 3.
func ParsePriority(s string) (int, bool)
// PriorityLabel is "P1" to "P3"; a zero or unknown value reads "P2".
func PriorityLabel(p int) string
```

  - `task.Normalize` turns a zero `Priority` into 2 and an empty `Agent` into `claude`. `task.Validate` reports `{Field: "priority", Problem: "must be P1, P2 or P3"}` for a value outside 1 to 3 and `{Field: "agent", Problem: "must be claude or codex"}` for any other agent, each in field order after the existing fields.
  - `curia task add … [--priority <p>]` and `curia task edit <id> … [--priority <p>]`; a value `ParsePriority` refuses is an error naming the flag, before the store is touched. `curia task ls` gains the `PRI` column after `ID`; `--json` gains `"priority"` (a number) and `"agent"`.

- [ ] **Step 1: Write the failing tests**

```go
// internal/task/task_test.go
func TestParsePriority(t *testing.T)               // "P1" 1, "p3" 3, "2" 2; "", "P0", "P4", "high", "P 1", "１" -> false
func TestPriorityLabel(t *testing.T)               // 1 "P1"; 0, 2, 9 "P2"; 3 "P3"
func TestNormalizeDefaultsPriorityAndAgent(t *testing.T)
func TestValidateReportsPriorityAndAgent(t *testing.T) // priority 4 and agent "gemini" together with an empty title -> three problems, title first
// cmd/curia/task_test.go
func TestTaskAddPriority(t *testing.T)             // --priority P3 -> stored 3; no flag -> 2; --priority urgent -> an error naming --priority, no task created
func TestTaskEditPriority(t *testing.T)            // P2 to P1; nothing else changes
func TestTaskLsShowsPriority(t *testing.T)         // the PRI column reads P1, P2, P3; the JSON golden file, edited by hand, matches
func TestTaskAddPriorityInsideAgent(t *testing.T)  // CLAUDECODE=1 -> works: task is the agent-facing CLI, and priority changes nothing by itself
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/task/ ./cmd/curia/ -run 'Priority|TaskLs' -v`. Expected: FAIL, undefined `ParsePriority`.
- [ ] **Step 3: Implement** and edit the golden file by hand.
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/task/ ./cmd/curia/ ./e2e/ -v`. Expected: PASS; where an earlier acceptance golden file lists a task object, the two keys are added to it by hand.
- [ ] **Step 5: Commit.** `git commit -m "feat: task priority P1 to P3"`

**Done when:** the eight tests pass.

### Task 16: Linear import

**Model:** Opus (the network, a secret, untrusted text into the store).

**Needs:** tasks: 5, 7. **Touches:** `cmd/curia/task_import.go`, `task_import_test.go`.

**Files:**

- Modify: `cmd/curia/task_import.go`
- Test: `cmd/curia/task_import_test.go`

**Brief includes:** section 4.9, section 2's "Secrets" and "Nothing leaves the machine", and section 3's item 6.

**Interfaces:**

- Consumes: `linear.Client`, `Issue`, `ValidKey`, `ValidTeam` (Task 5); `store.ImportTask` with priority (Task 7; E4); `task.Normalize`, `Validate`, `MaxBody` (E5); `config.Linear` (Task 1); the repository resolution of `task import --backlog-md` (E6); `sanitize.Line`, `sanitize.Block`.
- Produces, in package `main`:

```go
// linearTask maps one issue to a task for repo and base; ok is false when the issue must be skipped.
func linearTask(i linear.Issue, repo, base string, now time.Time) (t model.Task, ok bool)
// linearImport fetches and stores; it returns the three counts of the summary line.
func linearImport(ctx context.Context, st *store.Store, c linear.Client, team, repo, base string, now time.Time) (imported, updated, skipped int, err error)
```

  - `curia task import --linear [--repo <dir>] [--team <KEY>]`: exactly one of `--backlog-md` and `--linear` must be given. The key is `os.Getenv("CURIA_LINEAR_API_KEY")`; the client is `linear.Client{URL: cfg.Linear.URL, Key: key}`; the team is the flag, else `cfg.Linear.Team`. A missing or malformed key, or a bad team, exits 1 with section 4.9's message before any connection. A client error exits 1 with `linear: …` and stores nothing from the failed page onward (issues of earlier pages stay imported). Prints `imported 3, updated 2, skipped 1`.

Rules: section 4.9's mapping, exactly. An issue that `task.Validate` still refuses after `Normalize` is skipped and counted. The whole command has a 2 minute limit.

- [ ] **Step 1: Write the failing tests** (a local `httptest.Server` serving Task 5's built pages; the config file's `linear.url` points at it; a real git repository for `--repo`)

```go
func TestLinearImportCreatesTasks(t *testing.T)      // -> "imported 2, updated 0, skipped 1" (the malformed identifier); tasks with source linear, source_ref "ENG-12", status backlog, the repo's main top level and checked-out base
func TestLinearImportMapsFields(t *testing.T)        // the body starts "Linear ENG-12: https://linear.app/…" then a blank line then the description; labels "Bug", "Needs Design" -> tags bug, needs-design; Linear priority 1 -> P1, 0 -> P2, 4 -> P3
func TestLinearImportAgainUpdatesOnly(t *testing.T)  // a second run with a changed title -> "imported 0, updated 2, skipped 1"; status, priority and branch of the existing tasks untouched
func TestLinearImportCleansText(t *testing.T)        // a title of escape sequences and 300 runes; a 1 MiB description; a url "javascript:alert(1)"; a label "x\x1b[2J" -> stored clean and bounded; the url line omitted; no control rune in any stored field
func TestLinearImportNeedsKey(t *testing.T)          // unset; "a b" -> exit 1 with the message; the server saw no request
func TestLinearImportNeverPrintsTheKey(t *testing.T) // a server answering 401 and one echoing the header in a GraphQL error -> neither stdout, stderr nor the log holds the key
func TestLinearImportTeamFromFlagAndConfig(t *testing.T) // the flag wins; "eng" -> an error naming --team
func TestLinearImportServerDown(t *testing.T)        // a closed port -> exit 1, "linear: …", no task created
func TestLinearImportSecondPageFails(t *testing.T)   // page 1 fine, page 2 a 500 -> exit 1; page 1's tasks exist
func TestImportNeedsExactlyOneSource(t *testing.T)   // neither flag, both flags -> a usage error
func TestLinearStatusIsNeverMapped(t *testing.T)     // an issue whose state type is "started" (served although the filter excludes it) -> imported as backlog; no request holds "mutation"
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./cmd/curia/ -run 'Linear|ImportNeeds' -v`. Expected: FAIL, unknown flag `--linear`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./cmd/curia/ -v`. Expected: PASS, the Backlog.md import tests unchanged.
- [ ] **Step 5: Commit.** `git commit -m "feat: import Linear issues as tasks, read-only"`

**Done when:** the eleven tests pass.

### Task 17: Spawn and launch: agent and cheaper model

**Model:** Opus (a process boundary; what runs in a new window).

**Needs:** tasks: 3, 6, 7. **Touches:** `cmd/curia/spawn.go`, `spawn_test.go`, `launch.go`, `launch_test.go`.

**Files:**

- Modify: `cmd/curia/spawn.go`, `cmd/curia/launch.go`
- Test: `cmd/curia/spawn_test.go`, `cmd/curia/launch_test.go`

**Brief includes:** sections 4.4 ("Cheaper model") and 4.10 ("Spawn"), and section 3's item 2.

**Interfaces:**

- Consumes: `spawnOptions`, `spawnTask`, `spawnLine`, `launchExec`, `launchLookPath` (E7); `usage.Forecasts`, `Warning`, `BadgeText`, `BurnWindow` (Task 3); `store.UsageSince` (Task 6); `model.Task.Agent`, the stored agent (Tasks 1, 7); `config.Usage` (Task 1); `task.ValidModel`.
- Produces, in package `main`:
  - `spawnOptions` gains `Agent string` ("`claude`, `codex`, or empty: the task's own, `claude` for a task never spawned").
  - `spawnTask`: an agent other than `claude` or `codex` is an error; `codex` with `Team` is `agent teams are Claude Code's; spawn T-42 without --team or without --agent codex`; the agent is stored on the task with the pane (the same `UpdateTask` that stores `--team` and `--model`). A resume keeps the task's stored agent whatever the option says.
  - `spawnModel(ctx context.Context, st *store.Store, cfg config.Config, t model.Task, o spawnOptions, now time.Time) (model string, note string)`: section 4.4's "Cheaper model" rule. It returns `o.Model` and no note when that is set; the task's `AgentModel` when that is set; otherwise, for Claude Code, with a warning in force and a non-empty `cfg.Usage.CheaperModel`, that model and the note; otherwise an empty model. A failed usage read gives an empty model and no note (logged). `spawnResult` gains `Note string`, and `curia spawn` prints it before `spawnLine`.
  - `curia spawn <id> [--agent claude|codex] [--model <m>] [--team]`.
  - `curia launch`: for a task whose agent is `codex` it finds `codex` with `launchLookPath` and replaces itself with `codex [--model <m>] <brief>`, or `codex resume <uuid>` when resuming, in the same environment rule as before (`CURIA_TASK_ID`, never the team switch). For `claude` nothing changes.

- [ ] **Step 1: Write the failing tests** (real git repositories; `useFakeTmux`; a recording `launchExec`)

```go
// spawn_test.go
func TestSpawnDefaultsToCheaperModelUnderWarning(t *testing.T) // the warning series ingested; spawn with no --model -> the window command holds "--model", "sonnet"; the task's agent_model is "sonnet"; the output's first line is the note of section 4.4
func TestSpawnModelFlagBeatsForecast(t *testing.T)             // --model opus under the same warning -> opus, no note
func TestSpawnKeepsTaskModelUnderWarning(t *testing.T)         // a task spawned earlier with --model opus -> opus
func TestSpawnNoCheaperModelWithoutWarning(t *testing.T)       // a steady series; no samples -> no --model in the command, no note
func TestSpawnCheaperModelOff(t *testing.T)                    // usage.cheaper_model = "" -> no --model even under a warning
func TestSpawnResumeIgnoresForecast(t *testing.T)              // spawnOptions.Resume set -> the model rule of before
func TestSpawnUsageReadFailureDoesNotBlock(t *testing.T)       // usage_samples dropped by raw SQL -> the spawn succeeds with no model
func TestSpawnAgentCodexIsStored(t *testing.T)                 // --agent codex -> tasks.agent "codex"; the window command is still [<bin>, "launch", <id>]
func TestSpawnAgentRefusals(t *testing.T)                      // --agent gemini; --agent codex --team -> an error each, no worktree, no window, the claim released
func TestSpawnCodexNeverGetsCheaperModel(t *testing.T)
// launch_test.go
func TestLaunchCodexPassesBriefByExec(t *testing.T)            // a codex task -> launchExec got [<codex path>, <brief>] with CURIA_TASK_ID in the environment; the brief is one argument, byte-equal to task.Brief
func TestLaunchCodexModelAndResume(t *testing.T)               // --model o3 -> the argv is [codex, "--model", "o3", <brief>]; --resume <uuid> -> [codex, "resume", <uuid>]
func TestLaunchCodexNeverAddsSandboxOrApprovalFlags(t *testing.T) // no argument starts with "--dangerously", "--approve", "-s" or "--sandbox"
func TestLaunchCodexMissing(t *testing.T)                      // codex not found -> the one sanitized line, wait for Enter, exit 1, as for a missing claude
func TestLaunchClaudeUnchanged(t *testing.T)                   // a claude task's argv equals the one of before this task
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./cmd/curia/ -run 'Spawn|Launch' -v`. Expected: FAIL, unknown field `Agent`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./cmd/curia/ ./e2e/ -v`. Expected: PASS, Plan 3's and Plan 4's spawn, resume and restore tests unchanged.
- [ ] **Step 5: Commit.** `git commit -m "feat: spawn with --agent codex; cheaper model under a limit forecast"`

**Done when:** the fifteen tests pass.

### Task 18: `curia export`

**Model:** Sonnet (a command over pure rules; the file-system cases are named tests).

**Needs:** tasks: 4, 7. **Touches:** `cmd/curia/export.go`, `export_test.go`, `decide.go`, `decide_test.go`.

**Files:**

- Create: `cmd/curia/export.go`
- Modify: `cmd/curia/decide.go` (one call after an answer)
- Test: `cmd/curia/export_test.go`, `cmd/curia/decide_test.go`

**Brief includes:** sections 4.7 ("Finding an existing note", "Writing", "When", "Output") and 4.8, and section 3's item 5.

**Interfaces:**

- Consumes: `export.NoteName`, `IsNoteOf`, `OwnsNote`, `Note`, `CommonsEntry`, `ErrNotPublishable` (Task 4); `store.ExportQueue`, `MarkExported`, `GetDecision` (Task 7 and `main`); `config.Obsidian` (Task 1); `decision.ValidID`; `loadConfig`, `openStore`, `logWarn`, `clock`.
- Produces, in package `main`:

```go
// exportResult counts one sweep.
type exportResult struct {
    Written int
    Foreign []string // ids whose note exists without Curia's marker
}
// exportObsidian writes the notes that are due (all: every decided decision; ids: only those) into dir and marks them.
func exportObsidian(ctx context.Context, st *store.Store, dir string, all bool, ids []string, now time.Time) (exportResult, error)
// exportAfterAnswer runs the due sweep when obsidian.dir is set; it returns "" or the one line to show.
func exportAfterAnswer(ctx context.Context, st *store.Store, cfg config.Config, logPath string, now time.Time) string
```

  - `curia export obsidian [--all] [<id>…]` and `curia export commons <id>`, both under one `export` command registered in this file. `obsidian` prints section 4.7's output lines; `commons` prints the entry as one indented JSON object on stdout (HTML escaping off) and the warning line on stderr.
  - `cmd/curia/decide.go`: after a flag-form answer succeeded, and when the popup closes after at least one answer, `exportAfterAnswer` runs; the flag forms print its line when it is not empty; the popup prints nothing.

Rules: section 4.7, exactly. The folder is opened once with `os.OpenRoot`; listing reads at most 4096 entries; an existing candidate's first 4 KiB are read to look for the marker; a candidate that is not a regular file (a directory, a symlink) is treated as foreign. The write is `root.OpenFile(<temp name>, O_WRONLY|O_CREATE|O_EXCL, 0o600)`, write, close, `root.Rename(<temp>, <name>)`; a failed write removes its own temp file through the root and nothing else. A note that fails does not stop the sweep: it is counted and logged, and its decision is not marked. At most 500 notes a run.

- [ ] **Step 1: Write the failing tests** (a real folder under `t.TempDir()`)

```go
// export_test.go
func TestExportWritesDueNotes(t *testing.T)          // two answered decisions and one open -> two files named by export.NoteName, content equal to export.Note, mode 0600; "exported 2 notes to <dir>"; both marked
func TestExportSecondRunWritesNothing(t *testing.T)  // -> "nothing to export"; the files' modification times unchanged
func TestExportAgainAfterSupersede(t *testing.T)     // the note is rewritten with status superseded
func TestExportKeepsMyRename(t *testing.T)           // "D-1 Postgres….md" renamed by hand to "D-1 database choice.md" -> --all rewrites that file and creates no second one
func TestExportLeavesForeignNote(t *testing.T)       // a hand-written "D-2 my own notes.md" without the marker -> byte-equal afterwards; the output names D-2 as foreign; D-2 is not marked
func TestExportStaysInsideFolder(t *testing.T)
// a decision whose question is "../../escape"; a folder holding a symlink "D-3 x.md" -> /etc/hosts (a temp file standing in);
// a folder that is itself a symlink to another temp directory -> nothing is created or changed outside the resolved folder, the symlinked candidate is foreign
func TestExportNeverDeletes(t *testing.T)            // a directory snapshot before and after --all: no name disappeared except the temp names the run itself created
func TestExportByID(t *testing.T)                    // export obsidian D-1 -> only D-1; D-99 -> an error line for it, exit 1; an open decision named -> "D-4 is not decided"
func TestExportFolderUnsetOrMissing(t *testing.T)    // obsidian.dir "" -> exit 1 "obsidian.dir is not set"; a path that is not a directory -> exit 1 naming it; nothing created
func TestExportBound(t *testing.T)                   // 600 decided decisions -> one run writes 500; the next writes 100
func TestExportCommonsPrintsEntry(t *testing.T)      // a decided global decision -> stdout is the JSON of export.CommonsEntry; stderr is the warning line; no file written, no connection (a listener the test owns saw none)
func TestExportCommonsRefusals(t *testing.T)         // a repo-scope decision; an open one; an unknown id -> exit 1 with its line
// decide_test.go
func TestDecideExportsAfterAnswer(t *testing.T)      // obsidian.dir set: curia decide D-1 --option B --reason x -> the note exists with the answer and the reasoning
func TestDecideWithoutObsidianWritesNothing(t *testing.T)
func TestDecideExportFailureDoesNotFailTheAnswer(t *testing.T) // the folder made read-only (0500) -> the answer is stored and delivered, exit 0, the output holds "obsidian export failed: see the log"
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./cmd/curia/ -run 'Export|DecideExports|DecideWithoutObsidian' -v`. Expected: FAIL, unknown command `export`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./cmd/curia/ ./e2e/ -v`. Expected: PASS, Plan 2's decide tests and acceptance test unchanged.
- [ ] **Step 5: Commit.** `git commit -m "feat: export decisions to Obsidian; print an Agent Commons entry"`

**Done when:** the fifteen tests pass.

### Task 19: Review: branch cost

**Model:** Opus (a process boundary, a goroutine, a view).

**Needs:** tasks: 2. **Touches:** `cmd/curia/costlens.go`, `costlens_test.go`, `review.go`, `review_test.go`, `internal/ui/review.go`, `review_view.go`, `review_test.go`.

**Files:**

- Create: `cmd/curia/costlens.go`
- Modify: `cmd/curia/review.go` (the source's `Detail` and the `--json` object), `internal/ui/review.go` (`ReviewDetail`), `internal/ui/review_view.go` (line 7)
- Test: `cmd/curia/costlens_test.go`, `cmd/curia/review_test.go`, `internal/ui/review_test.go`

**Brief includes:** sections 4.6 and 4.5's "Review" item, and section 3's item 10.

**Interfaces:**

- Consumes: `usage.BranchCost` (Task 2); `config.CostLens` (Task 1); `ui.ReviewDetail`, the review view's line 7, the review source (E11); `usage.CostCell` is not used here (Task 3 is not needed): the view formats with the review's existing cost format.
- Produces:

```go
// cmd/curia/costlens.go
var costlensLookPath = exec.LookPath
// costlensRun runs the analyzer and returns its stdout, cut at 4 MiB. Tests replace it.
var costlensRun = func(ctx context.Context, node, analyzer, repo string) ([]byte, error)
const costlensLimit = 90 * time.Second
const costlensKeep = 5 * time.Minute
// costlensAnalyzer finds node and the analyzer script (section 4.6); ok is false when either is missing.
func costlensAnalyzer(cfg config.Config) (node, analyzer string, ok bool)
// costlensCache answers branch-cost questions without ever blocking: the first Get for a repository starts one run.
type costlensCache struct{ /* unexported; a mutex, the kept summaries, the running flag */ }
func newCostlensCache(cfg config.Config, logPath string, now func() time.Time) *costlensCache
// Get returns the branch's cost when a summary for repo younger than costlensKeep is held; otherwise it starts a run (unless one is running) and returns false.
func (c *costlensCache) Get(repo, branch string) (usd float64, ok bool)
// Wait blocks until no run is in flight or ctx ends; curia review --json uses it once, with costlensLimit.
func (c *costlensCache) Wait(ctx context.Context)
```

  - `ui.ReviewDetail` gains `BranchCostUSD float64` and `BranchCostKnown bool`. The view's line 7 appends ` · branch $5.03 (cost-lens)` when known, in the same number format as the session cost; nothing otherwise.
  - The review source holds one `costlensCache`; `Detail` calls `Get(task.Repo, task.Branch)` for a task with a branch. `curia review <id> --json` calls `Get`, then `Wait`, then `Get` again, and prints `branch_cost_usd` as a number or null.

Rules: section 4.6, exactly. One run at a time across the whole cache. The run's context is detached from the caller's and bounded by `costlensLimit`; `cmd.WaitDelay` is 2 seconds; stdin is `/dev/null`; stderr is discarded; `--live-prices` is never passed. A failed run is logged once at WARN with the error's kind and remembered as "no answer" for `costlensKeep`, so a broken analyzer is not started every 5 seconds. The repository path and the branch are never part of a shell string.

- [ ] **Step 1: Write the failing tests**

```go
// cmd/curia/costlens_test.go
func TestCostlensAnalyzerFromConfig(t *testing.T)     // costlens.analyzer set to a real file and a stub node on PATH -> those two paths
func TestCostlensAnalyzerBesideTheLink(t *testing.T)  // a temp package dir with terminal/cli.js and analyzer/cli.js, a "cost-lens" symlink to the first on PATH -> the second; without analyzer/cli.js, or with a link target of another name -> ok false
func TestCostlensAnalyzerMissingNode(t *testing.T)    // -> ok false
func TestCostlensGetStartsOneRun(t *testing.T)        // a costlensRun that blocks on a channel: ten Gets -> one run started, every Get false; released with summary-built.json -> Get true, 5.03
func TestCostlensRunIsBounded(t *testing.T)           // the real costlensRun with a stub "node" that sleeps: limit shortened to 200 ms -> returns within 3 s with an error; a stub printing 5 MiB -> at most 4 MiB read, BranchCost false
func TestCostlensArgvIsDirect(t *testing.T)           // the real costlensRun with a stub node that records its argv: [<analyzer>, "--project", <repo>], no "--live-prices"; a repo path holding spaces and ";" is one argument
func TestCostlensFailureIsRemembered(t *testing.T)    // a failing run -> the next Get within costlensKeep starts nothing; after it, one new run
func TestCostlensKeepExpires(t *testing.T)            // an injected clock past costlensKeep -> a new run
// cmd/curia/review_test.go
func TestReviewDetailNeverWaits(t *testing.T)         // a blocking costlensRun -> Detail returns within 100 ms with BranchCostKnown false; after release the next Detail has it
func TestReviewJSONHasBranchCost(t *testing.T)        // -> "branch_cost_usd": 5.03; with no analyzer -> null, and the command does not wait
func TestReviewWithoutBranchAsksNothing(t *testing.T) // a task with no branch -> costlensRun never called
// internal/ui/review_test.go
func TestReviewShowsBranchCost(t *testing.T)          // known -> line 7 ends "· cost $4.12 · branch $5.03 (cost-lens)"; not known -> the line of Plan 4, byte for byte
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./cmd/curia/ ./internal/ui/ -run 'Costlens|BranchCost|ReviewDetailNeverWaits|ReviewWithoutBranch' -v`. Expected: FAIL, undefined `costlensCache`.
- [ ] **Step 3: Implement.** The test file replaces `costlensRun` and `costlensLookPath` with defaults that fail any test which has not set its own.
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./cmd/curia/ ./internal/ui/ ./e2e/ -v`. Expected: PASS, Plan 4's review tests and acceptance test unchanged.
- [ ] **Step 5: Commit.** `git commit -m "feat: branch cost from the cost-lens analyzer in a review"`

**Done when:** the twelve tests pass under `-race`.

### Task 20: Dashboard: cost, cache, burn sort

**Model:** Opus (UI).

**Needs:** tasks: 3, 6, 12. **Touches:** `internal/ui/model.go`, `view.go`, `ui_test.go`, `cmd/curia/ui.go`, `ui_test.go`. Merges before Task 21.

**Files:**

- Modify: `internal/ui/model.go`, `internal/ui/view.go`, `cmd/curia/ui.go`
- Test: `internal/ui/ui_test.go`, `cmd/curia/ui_test.go`

**Brief includes:** section 4.5's "Dashboard (popup)" item and section 4.4's "Cells".

**Interfaces:**

- Consumes: `usage.CostCell`, `CacheCell`, `Burns`, `Forecasts`, `Warning`, `BadgeText`, `APIProjection`, `BurnWindow` (Task 3); `store.UsageSince`, `CostSince`, the session's cost and cache fields (Tasks 1, 6); `fleet.Unit.Cost`, `CacheOf` (Task 12); the dashboard as E10 describes it; `storeSource` (`cmd/curia/ui.go`).
- Produces:

```go
// internal/ui/model.go
// Budget is what the dashboard shows about usage. Every string in it is already rendered by Curia.
type Budget struct {
    Status string             // the forecast warning or the API projection; "" when there is none
    Burn   map[string]float64 // USD per hour by session id; nil when unknown
}
```

  - `ui.Source` gains `Budget(ctx context.Context) (Budget, error)`; only the popup calls it, with each refresh. A failure keeps the last good value and shows the error on the status line, as `OpenDecisions` does.
  - Columns `COST` and `CACHE` after `DEC`, at 100 columns or more, as section 4.5 says; a group row uses `Unit.Cost` and `Unit.CacheOf`. `b` toggles the burn sort; it is not a command while the filter input is open, and pasted text never toggles it. The help line gains `b burn`.
  - `cmd/curia/ui.go`: `storeSource.Budget` builds `Status` (the warning's `usage.BadgeText`, else `next 2h ≈ $<x> at the current rate` in API mode) and `Burn` from the two sample reads.

Rules. The selection follows the session id across a re-sort, as it does across a refresh. With the burn sort on, units with no burn keep their rank order after those that have one. The sidebar shows neither column and ignores `b`. Width rules below 100 columns are exactly Plan 3's.

- [ ] **Step 1: Write the failing tests**

```go
// internal/ui/ui_test.go
func TestCostAndCacheColumns(t *testing.T)          // width 120: an idle session with cost 4.12 and a cache expiring in 12m -> "$4.12" and "12m" under COST and CACHE; a working session -> its cost and an empty CACHE
func TestCostAndCacheHiddenBelow100(t *testing.T)   // width 99 -> the columns of Plan 3 at that width, unchanged
func TestCacheCellStyles(t *testing.T)              // 4m -> the warning style; cold -> dim; through the style function, as decStyle is tested
func TestGroupRowSumsCostAndShowsSoonestCache(t *testing.T) // a team of three -> the sum; the soonest warm expiry among its idle members
func TestBKeyTogglesBurnSort(t *testing.T)          // burns s1 0.5, s2 3.0, s3 none -> rows s2, s1, s3; the header shows "sorted by burn (b)"; b again -> the rank order
func TestBurnSortKeepsSelection(t *testing.T)       // the selected session is still selected after b
func TestBurnSortGroupsByUnit(t *testing.T)         // a team's members' burns are summed and the team moves as one row
func TestBIsTextInTheFilter(t *testing.T)           // "/" then "b" -> the filter holds "b", the sort is unchanged
func TestPasteDoesNotToggleBurn(t *testing.T)
func TestBudgetStatusLine(t *testing.T)             // Budget.Status "5h 62% → limit ~48m (resets 1h50m)" -> on the status line; "" -> the status line of before
func TestBudgetErrorKeepsLastValue(t *testing.T)
func TestSidebarHasNoCostColumns(t *testing.T)      // and never calls Budget
func TestCostTextCannotDrawEscapes(t *testing.T)    // a Budget.Status holding "\x1b[2J" (a faulty source) -> not drawn raw
// cmd/curia/ui_test.go
func TestStoreSourceBudget(t *testing.T)            // the warning series and rising costs ingested -> Status is the badge text, Burn has the sessions; no samples -> a zero Budget
func TestStoreSourceBudgetAPIMode(t *testing.T)     // -> Status "next 2h ≈ $9.00 at the current rate"
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/ui/ ./cmd/curia/ -run 'Cost|Cache|Burn|Budget|BKey|BIs' -v`. Expected: FAIL, `Source` has no method `Budget`.
- [ ] **Step 3: Implement.** Every existing test's source gains a `Budget` that returns the zero value.
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/ui/ ./cmd/curia/ -v`. Expected: PASS, every earlier dashboard test unchanged at widths below 100.
- [ ] **Step 5: Commit.** `git commit -m "feat: cost and cache columns and burn sort on the dashboard"`

**Done when:** the fifteen tests pass.

### Task 21: Dashboard: pause suggestion

**Model:** Opus (UI and a destructive action behind a confirmation).

**Needs:** tasks: 7, 14, 20. **Touches:** `internal/ui/model.go`, `view.go`, `ui_test.go`, `cmd/curia/ui.go`, `ui_test.go`, `pause.go`.

**Files:**

- Modify: `internal/ui/model.go`, `internal/ui/view.go`, `cmd/curia/ui.go`, and `cmd/curia/pause.go` only as E8 says
- Test: `internal/ui/ui_test.go`, `cmd/curia/ui_test.go`

**Brief includes:** section 4.5's "Pause suggestion" item, section 4.4's "Pause suggestion", section 2's "Only I decide, approve, merge or pause", and section 3's item 2.

**Interfaces:**

- Consumes: `ui.Budget`, `Source.Budget` (Task 20); `usageBuild`, `usageReport` (Task 14); the confirm prompt and dwell of the dashboard's `x` (E10); `curia pause` (E8); `refuseInsideAgent`.
- Produces:
  - `ui.Budget` gains `PauseTask`, `PauseLabel`, `PauseTitle string` (empty: no suggestion).
  - `ui.Source` gains `Pause(ctx context.Context, taskID string) (line string, err error)`.
  - `func (m Model) WithPause() Model` enables `P`; without it `P` says why not (`pausing is not available here`). The notice line, the prompt and its rules are section 4.5's.
  - `cmd/curia/pause.go`, when it has no such function yet: `type pauseOptions struct{ Force, NoVerify bool }` and `func pauseTask(ctx context.Context, st *store.Store, tm tmux.Client, cfg config.Config, id string, o pauseOptions) (lines []string, err error)`, the body of the command moved, the command calling it and printing the lines; Plan 3's lifecycle tests pass unchanged.
  - `cmd/curia/ui.go`: `storeSource.Budget` now takes all its fields from `usageBuild`; `storeSource.Pause` calls `pauseTask` without force and returns its last line; `runDashboard` sets `WithPause` in popup mode when `refuseInsideAgent` passes.

Rules. The suggested task shown in the prompt is the one the notice named when `P` was pressed; if a refresh changes or removes the suggestion while the prompt is open, the prompt is canceled with `the suggestion changed`. `y` counts only 250 ms after the prompt opened, measured against a held key as `x`'s confirm is; a paste never confirms. `Pause` runs once per confirmation. A pause that fails shows git's or the command's cleaned error as the notice and changes nothing in the view. Nothing is ever paused without the two keys.

- [ ] **Step 1: Write the failing tests**

```go
// internal/ui/ui_test.go
func TestPauseNoticeShown(t *testing.T)               // Budget with PauseTask T-7 -> the notice line of section 4.5; without -> no line
func TestPauseNeedsKeyThenConfirm(t *testing.T)       // P opens the prompt and calls nothing; y after the dwell calls Pause("T-7") exactly once; the result line becomes the notice
func TestPauseAnyOtherKeyCancels(t *testing.T)        // P then n, Esc, Enter, P -> no call
func TestPauseConfirmHonorsDwell(t *testing.T)        // P and y in one read; y 100 ms after P -> no call
func TestPausePasteDoesNotConfirm(t *testing.T)       // a paste "Py" and a paste "y" into the open prompt -> no call
func TestPausePromptCanceledWhenSuggestionChanges(t *testing.T) // a refresh naming T-9, or none, while the prompt for T-7 is open -> canceled, no call on a following y
func TestPauseWithoutSuggestion(t *testing.T)         // P with no PauseTask -> "nothing to pause: no limit forecast"
func TestPauseDisabledInSidebarAndWithoutWithPause(t *testing.T)
func TestPauseErrorShown(t *testing.T)                // Pause returns an error holding "\x1b[31m" -> shown cleaned; a second P works
func TestPauseNeverCalledWithoutKeys(t *testing.T)    // a hundred refreshes with a suggestion and no key -> zero calls
// cmd/curia/ui_test.go
func TestStoreSourceBudgetSuggestsPause(t *testing.T) // the warning series; running tasks P2 and P3 -> PauseTask is the P3 one, with label and title
func TestStoreSourcePauseRunsPause(t *testing.T)      // a spawned task in a real git repository with useFakeTmux -> its worktree is removed, its branch kept, paused_at set; the returned line is the pause's last
func TestDashboardOffersPauseInPopupOnly(t *testing.T) // popup mode outside an agent -> WithPause set; sidebar, and CLAUDECODE=1 -> not set
func TestPauseCommandUnchangedAfterMove(t *testing.T) // curia pause T-1 prints the same lines as before this task (only when pause.go was edited)
```

- [ ] **Step 2: Run and confirm they fail.** Run: `go test ./internal/ui/ ./cmd/curia/ -run 'Pause' -v`. Expected: FAIL, `Source` has no method `Pause`.
- [ ] **Step 3: Implement.** Every existing test's source gains a `Pause` that fails the test when called.
- [ ] **Step 4: Run and confirm they pass.** Run: `go test -race ./internal/ui/ ./cmd/curia/ -v`. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat: suggest pausing the lowest-priority task under a limit forecast"`

**Done when:** the fourteen tests pass.

## 11. Task 22 (acceptance) and Task 23 (V1 docs, orchestrator)

### Task 22: End-to-end acceptance

**Model:** Opus (cross-process acceptance).

**Needs:** tasks: 1 to 21. Task 10 may still be held for its probes: step 10's install part then runs under `t.Skip` naming probe X1, and the rest of the test is complete without it. **Touches:** `testdata/cost/`, `e2e/cost_test.go`, `e2e/testdata/cost/`.

**Files:**

- Create: `testdata/cost/` (`ingest.json.tmpl`, `ingest_garbage.txt`, `statusline.json`, `linear_page1.json`, `linear_page2.json`, `codex_session_start.json`, `summary.json`, `node-stub.sh`, `codex-stub.sh`, `config.toml.tmpl`, `client_agent.jsonl`), `e2e/cost_test.go`, `e2e/testdata/cost/` (`usage.golden.json`, `note_d1.golden.md`, `commons_d2.golden.json`, `tasks.golden.json`, `push_d3.golden.json`, `stub_argv_t3.golden.txt`, `codex_argv_t7.golden.txt`, `review_t1.golden.json`)

**Brief includes:** sections 4.2 to 4.10.

**Interfaces:**

- Consumes: the built `curia` binary: `install`, `hook`, `mcp`, `ingest`, `statusline`, `status`, `usage`, `task`, `spawn`, `decide`, `decisions`, `export`, `review`, `ls --json`.
- Produces: the Plan 5 acceptance test.

**Harness.** `e2e/cost_test.go` reuses the helpers of the earlier acceptance tests (E16), reading them and editing none; it stats its own fixtures so a cached pass is not reused after they change. It imports no internal package. It adds:

- A temp git repository `api-server` on `main` with one commit.
- Two loopback HTTP listeners owned by the test: a stand-in Linear that serves `linear_page1.json` then `linear_page2.json` and records every request, and Plan 4's stand-in ntfy. `config.toml.tmpl` is filled with both, with `[obsidian] dir` set to a temp folder and `[costlens] analyzer` set to a file in the temp `HOME`, and written to the temp `HOME`'s `.config/curia/config.toml`.
- A `bin` directory first on `PATH` holding the stub `claude` of Plan 3, `codex-stub.sh` as `codex` (it records its argv and draws nothing), and `node-stub.sh` as `node` (it records its argv and prints `summary.json`).
- A private tmux server; every run of the binary gets `TMUX=<socket_path>,<pid>,0`, a temp `HOME`, `CURIA_DB`, `CLAUDE_CONFIG_DIR` and `CODEX_HOME`, `CURIA_LINEAR_API_KEY=e2e-key` only in step 9, and `CLAUDECODE` unset except where a step sets it.

**Script.**

1. `curia install` (stub `claude`): the temp `settings.json` holds Curia's `statusLine`; a second `curia install` prints `status line: already registered` and leaves the file byte-equal; `curia uninstall` then `curia install` again restores it.
2. `curia task add "Fix the auth refresh" --repo <repo>` (T-1), `curia task add "Tidy the docs" --repo <repo> --priority P3` (T-2); `curia spawn T-1` and `curia spawn T-2`: two stub agents, two sessions.
3. `echo garbage | curia ingest`, `curia ingest < /dev/null` and `curia ingest < ingest_garbage.txt` (1 MiB): each exits 0 with empty stdout and stderr; `curia usage` prints the "no usage has been ingested" line.
4. Three ingests for T-1's session from `ingest.json.tmpl`, with `at` 15, 8 and 1 minutes before the test's clock, five-hour usage 50, 56 and 62 with a reset 110 minutes ahead, costs 3.00, 3.60 and 4.12, a cache expiry 12 minutes ahead and a rebuild cost of 1.68; and three for T-2's session with costs 0.10, 0.20 and 0.30. `curia status` matches `^5h 62% → limit ~4[0-9]m \(resets 1h(49|50)m\)$`. `curia usage --json`, normalised, equals `usage.golden.json`: one warning window, T-1's session burning more than T-2's, `suggest_pause` T-2.
5. `curia statusline < statusline.json` prints exactly `Opus 5.5 · ctx 71% · $4.12 · 5h 62%`; with `CLAUDECODE=1`, `curia ingest --statusline < statusline.json` exits 0.
6. `curia task add "Add the audit log" --repo <repo>` (T-3) and `curia spawn T-3`: the first output line starts `limit forecast (5h 62%`; the stub's argv equals `stub_argv_t3.golden.txt` (`--model sonnet` before the brief). `curia task add "Rename the flag" --repo <repo>` (T-4) and `curia spawn T-4 --model opus`: no note, and the stub's argv holds `opus`.
7. An MCP client in T-1's pane raises a blocking ticket with options A, B, C and recommendation B (D-1), a second with scope `global` (D-2), and a third (D-3). Within 3 seconds the stand-in ntfy holds D-3's push equal to `push_d3.golden.json` after normalising: its message ends with a line matching `answer within 1[0-2]m to keep cache warm \(rebuild ≈ \$1\.68\)`.
8. `curia decide D-1 --option B --reason "one database to run"`: the Obsidian folder holds `D-1 <title>.md` equal to `note_d1.golden.md` after the date is normalised. `curia decide D-2 --option A --reason r`; `curia export commons D-2` prints `commons_d2.golden.json` and one line on stderr; `curia export commons D-1` exits 1. `curia export obsidian` prints `nothing to export`. A file `D-3 mine.md` written by the test without the marker is byte-equal after `curia decide D-3 --option A --reason r`, and the output of `curia export obsidian --all` names D-3 as foreign.
9. `curia task import --linear --repo <repo>` prints `imported 2, updated 0, skipped 1`; the stand-in Linear saw two requests, each with `Authorization: e2e-key`, a body whose `query` starts `query(` and holds no `mutation`; run again it prints `imported 0, updated 2, skipped 1`. `curia task ls --status all --json`, normalised, equals `tasks.golden.json` (priorities and agents included). No output, and no line of the log, holds `e2e-key`.
10. Codex. `curia hook SessionStart --agent codex < codex_session_start.json` in a fresh pane: `curia ls --json` shows that session with `agent` `codex`. `curia hook Stop --agent codex` for it prints nothing. `curia task add "Port the client" --repo <repo>` (T-7, after the two imported tasks), `curia spawn T-7 --agent codex`: the codex stub's argv equals `codex_argv_t7.golden.txt` (the brief as one argument, no flag). With Task 10 merged: `curia install --codex` with the stub `codex` writes the hooks file under the temp `CODEX_HOME` and calls `codex mcp add curia -- <bin> mcp`.
11. Review cost. The MCP client calls `complete_task` for T-1 after the test commits on its branch; `curia review T-1 --json`, normalised, equals `review_t1.golden.json`: `cost_usd` 4.12 and `branch_cost_usd` 5.03; the node stub's argv is `<analyzer> --project <repo>`.
12. Latency and silence. A separate sequential pass of 400 `curia ingest` processes on that database: p95 under 50 ms (Plan 1's method for the hook, Ruling 26). The earlier hook latency tests pass unchanged. The log holds no `level=ERROR` line.

**Normalised before comparing, and nothing else:** every RFC 3339 time becomes `"<time>"`; the temp repository's path (plain and symlink-resolved) becomes `<repo>`; the temp home becomes `<home>`; `args_hash` values become `<hash>`; the one-time token of a push becomes `TOKEN`; in `usage.golden.json` each `usd_per_hour` is rounded to one decimal (the costs and offsets of step 4 give 3.4 and 0.6).

- [ ] **Step 1: Write the failing test**

```go
func TestCostEndToEnd(t *testing.T)     // steps 1 to 11
func TestIngestProcessLatency(t *testing.T) // step 12
```

- [ ] **Step 2: Run and confirm it fails.** Run: `go test ./e2e/ -run 'Cost|IngestProcess' -v`. Expected: FAIL until the fixtures and golden files exist.
- [ ] **Step 3: Write the fixtures and golden files** from the script and section 4, by hand, never from the program's output.
- [ ] **Step 4: Run the whole suite.** Run: `make check`. Expected: every gate passes, the four earlier acceptance tests and every latency test included and unchanged.
- [ ] **Step 5: Whole-plan checks (orchestrator).** `deadcode ./...` without `-test` on `plan-5` reports only deliberate test doubles (`tmux.Fake`). Live check B of section 7 runs.
- [ ] **Step 6: Commit.** `git commit -m "test: end-to-end cost, forecast, export, import and Codex acceptance"`

**Done when:** `TestCostEndToEnd`, `TestIngestProcessLatency` and `make check` pass.

### Task 23: V1 docs and release notes (orchestrator)

**Owner:** the orchestrator, on `docs/v1-completion`, as the last PR into `plan-5` before `plan-5` goes into `main`. No implementer touches these files.

**Needs:** tasks: 22, the final whole-branch review and its fix wave. **Touches:** `README.md`, `CHANGELOG.md` (new), `AGENTS.md`, `PROGRESS.md`, `docs/v1-as-built.md` (new).

- [ ] **`README.md`.** The command table gains `curia ingest`, `curia statusline`, `curia usage`, `curia export obsidian`, `curia export commons`, `curia task import --linear`, `curia spawn --agent codex`, and the `--priority` flag. "What `curia install` changes" gains the status line (when it is added, when it is left alone, the `tee` hint, `--no-statusline`) and `--codex`. A section "Cost and usage": where the numbers come from (cost-lens when it pushes, else the status line), what the badge warning means, that nothing is paused or changed without a key, the config keys of section 4.11. A section "Integrations": Obsidian (the folder, what a note holds, that text is fenced and why), Linear (read-only, `CURIA_LINEAR_API_KEY`, the fixed filter), Agent Commons (printed, not sent), Codex (what works and what does not). The safety section gains: what leaves the machine and when (ntfy, `curia serve`, Linear; nothing else), and Plan 2's note that Claude Code labels a Stop-hook delivery "Stop hook error" (live check 2, defect 12). "Status" changes from "Pre-1.0" to V1 with the known limits of section 17.
- [ ] **`CHANGELOG.md`.** One entry, `1.0.0`, grouped by what I can now do, not by plan: sessions and attention; decisions and precedent; tasks, spawning, teams and messages; reviews, pushes, the phone endpoint and restore; cost, forecast and integrations. Then "Upgrading from 0.x": run `curia install` again (six tool rules, the status line, the marker's new value) and re-source the tmux snippet; a `curia mcp` started before the upgrade keeps its old tool list and keeps returning a superseded answer as live until its session restarts (Plan 2 re-review m6); the database migrates itself on first use and an older binary keeps working against it. Then "Known limits", from section 17.
- [ ] **`docs/v1-as-built.md`.** Every place where V1 differs from `docs/DESIGN.md`, one line each with its Ruling or plan decision: the schema additions of migrations 0001 to 0006, the badge format (Ruling 91), SessionStart as idle (Ruling 37), Curia's own worktrees (Plan 3 decision 1), per-device topics and one-time tokens (Plan 4 decision 16), dollars for token deltas, the printed Agent Commons entry, and the rest of section 17's second table. `docs/DESIGN.md` itself is not edited: it is the owner's (Plan 1 Open Question 20; question 9 of section 15).
- [ ] **`AGENTS.md`.** "Sources of truth": all five plans complete, `docs/v1-as-built.md` named. The toolchain rule about `go mod tidy` reworded for work after V1 (a new dependency needs a decision record). Everything section 13's probes and the live checks established.
- [ ] **`PROGRESS.md`.** The Plan 5 record: tasks, PRs, models, fix rounds, rulings, the probes' results, the results table as Plans 1 and 2 have it; section 17's two tables with each row's final state; a "After V1" list holding every item this plan and the earlier ones cut or left deferred.
- [ ] **Version.** Nothing in the tree carries the version: GoReleaser stamps it from the tag. The orchestrator does not push a tag. After `plan-5` has merged into `main` with CI green, pushing `v1.0.0` is the owner's step (it publishes a release and the Homebrew formula; question 10 of section 15); the changelog entry's text is ready to paste into the release.
- [ ] **Check.** `make check` passes (the layout check knows `CHANGELOG.md` since setup); every command named in the README exists in `curia --help`.

**Done when:** the five files are merged and `PROGRESS.md` says which rows of section 17 are verified by a test, which by a live check, and which are the owner's to confirm.

## 12. Decisions made in this plan

Each is: what — why — the alternative and the cost if wrong.

1. **One ingest path for cost-lens and for the status line** — the spec's "same ingest path"; one parser, one transaction, one place to bound input — a second source needs only a mapping function.
2. **Ingest writes three existing session columns and two sample tables; `cost_samples` is added to the spec's `usage_samples`** — the API math and the attribution need a session's cost over time, and the session row holds only the latest — one more table, pruned after 24 hours.
3. **Ingest ignores `model`, `context_used` and `context_max`** — F2 says they come from the transcript, and two writers of one column would disagree (the hook's value is per turn, the push's per turn end) — the alternative is letting ingest win; if wrong, three more `COALESCE` lines.
4. **An absent or null field never clears a stored value** — the status line carries no cache fields and must not erase what cost-lens pushed — a cache value can outlive its truth until the next push; an expiry in the past simply reads cold.
5. **The contract gains an optional `at`** — a reading has a time of its own, a pusher may deliver late, and the acceptance test needs a series it cannot wait 15 minutes for — a caller can backdate a reading by up to 24 hours; it can already send any number.
6. **`curia ingest` is allowed inside an agent session and trusts nothing it reads** — cost-lens calls it from inside Claude Code — an agent can forge usage numbers; the cost is a wrong warning, a wrong suggestion or a cheaper default model, each of which I see and can override, and nothing else.
7. **The status line is registered by default when I have none, and never replaces mine** — verified at the desk: cost-lens 0.3.0 has no push to Curia, so without the status line V1 would have no live numbers at all; the installer rulings forbid replacing what I set — it adds a line to Claude Code's screen that I did not ask for (question 1); `--no-statusline` opts out.
8. **Burn is measured over the trailing 20 minutes from at least two samples at least 3 minutes apart, merged across sessions, cut at a drop** — the spec's 20 minutes; one sample has no slope, and two samples seconds apart amplify rounding into a false alarm — a single long turn produces no sample and so no warning until it ends.
9. **No warning without a reset time** — the spec's condition is "before `resets_at`" — a source that omits reset times never warns.
10. **The badge carries the spec's full sentence and only the sooner of two warnings; no tmux option is changed for it** — Ruling 96 — on a short `status-right-length` tmux cuts it; the README says which option to raise.
11. **The API projection is shown by `curia usage` and the dashboard, not the badge** — the spec gives the badge a wording only for the subscription case, and a dollar figure with no limit to compare it to is not an alarm — API users must look.
12. **Attribution is in dollars per hour, not token deltas** — the ingest contract carries no token counter and Curia keeps no price table to convert one — a session on a cheap model looks smaller than its token share of a subscription window.
13. **Accepting the pause suggestion is `P` then `y`** — the spec says one key, but a pause commits work and closes a pane, and every such action here needs a confirmation after a dwell (Rulings 66 and 88 came from keys that acted too easily) — one key more than the spec (question 2).
14. **The suggestion considers only tasks that are `running`, and takes the lowest priority literally, even when everything is P1** — an agent waiting on me burns nothing; the spec says "lowest-priority running task" — it can suggest pausing the only thing running.
15. **The cheaper model is a configured name, default `sonnet`; cost-lens's advisor is not used** — the advisor lives inside the cost-lens mod and has no command or file to read (verified at the desk) — the alternative is a per-task choice; if wrong, one config line, and `--model` always wins.
16. **Priority is an integer 1 to 3 on the task, set by `task add` and `task edit`, shown in `task ls`; the board does not show or sort by it** — the forecast is its only reader in V1 — the board is one cell away.
17. **The branch cost in a review comes from the analyzer found beside the `cost-lens` link, run in the background and kept 5 minutes** — it took 12 seconds on this machine and cost-lens's own limit for it is 5 minutes, while the review refreshes every 5 seconds — the number appears a few seconds after the review opens; a branch name reused over time sums all its work.
18. **Curia never computes a cost** — the spec: one price table, in cost-lens — without cost-lens and without the status line every cost reads blank.
19. **Every agent- or user-written string in an Obsidian note is inside a plain code fence** — spec section 7: ticket content is never interpreted, and a vault with Dataview or Templater interprets a great deal — the notes are not pretty markdown, and links in my reasoning are not clickable (question 5).
20. **The Obsidian folder must exist; notes are written through `os.Root`; a note without Curia's marker is never overwritten; nothing is deleted** — the vault is mine and may be a symlink into a synced folder — a note I created by hand under a decision's id blocks that decision's export until I rename it.
21. **Notes are written by a sweep that runs after `curia decide` and on `curia export obsidian`** — one place that writes, no file writes from `curia serve` or `curia mcp` — an answer given from the phone reaches the vault at the next answer at the desk or the next sweep.
22. **An Agent Commons entry is printed, not sent** — verified at the desk: `propose` wants the proposing agent's own `task_id` and `model`, its entries are debugging knowledge (symptom, cause, fix) reviewed by a person, and the only transport is MCP over HTTP with a bearer key; a network client and a stored credential for a mapping this loose is not V1 work — publishing takes one more step (hand the JSON to an agent that has the commons tools); the direct publish is one task later (question 4).
23. **The Linear key comes only from `CURIA_LINEAR_API_KEY`, and the Linear URL only from the config file** — the owner's secrets live in the Keychain as environment variables; a URL from a flag is reported by `gosec` as G704 — a second workspace needs a second environment.
24. **The Linear filter is fixed: issues assigned to me in backlog or unstarted states, optionally of one team, at most 200** — the spec says "import of issues" and nothing about which; these are the ones I could start — importing a teammate's issue needs a code change.
25. **A re-import updates title, body and tags and leaves priority, status and everything spawn wrote** — Plan 3's `ImportTask` rule, and adapters "only map fields" (F5) — a priority changed in Linear after the first import is not picked up.
26. **Linear priority 1 and 2 become P1, 0 and 3 become P2, 4 becomes P3** — three levels against five — "no priority" reads as normal.
27. **Codex goes through the same hook with `--agent codex`; its transcript is not read and its Stop claims nothing** — the spec's "same hook adapter keyed by agent type"; the transcript format and the meaning of a Stop hook's output are Codex's own and unverified — a Codex row shows no model or context, and an answer reaches a Codex agent only when it calls `check_decision`.
28. **`curia install --codex` is opt-in and its task waits for probes X1 and X2** — it writes into another program's configuration in a format nobody has recorded yet — Codex support may land after the rest of Plan 5; nothing else depends on it.
29. **A Codex task is spawned and resumed through `curia launch`, with the agent stored on the task; an unlinked Codex session is not restored** — `launch` already decides what runs in the window; Plan 4's restore skips "not Claude Code" — a hand-started Codex session does not come back after a reboot.
30. **`curia ls` and `ls --json` do not change; `curia usage --json` carries cost and burn** — fewer golden files to touch and one place for the numbers — a script that wants a session's cost reads `curia usage --json`.
31. **The stakes line of a push is computed when the push is sent** — a push held through quiet hours would otherwise carry a countdown from the night before — none.
32. **The dashboard's cost and cache columns appear at 100 columns or more** — below that Plan 3 is already dropping columns — a narrow popup shows no cost; `curia usage` does.
33. **An identical reading within 60 seconds adds no sample, and each ingest prunes at most 200 old rows** — ten sessions report the same account-wide number at every turn end; a separate housekeeping command would never be run — the series has at most one point a minute per value.
34. **Plan 2's deferred minor about the decide view's `face` and the stakes line is left as it is** — the view recomputes the stakes text from the clock at every refresh, the selection follows the ticket id, and making a countdown count as "the ticket changed" would restart the dwell every minute — a warm-to-cold flip does not restart the dwell.
35. **No MCP tool is added, and the operator skill stays deferred** — the spec defers it, and `curia usage --json`, `task ls --json` and `ls --json` are what it would read — an operator agent is a skill file away.
36. **The cost-lens side of the push is not built here** — it is another repository; this plan ships the contract file, the command and a status-line source that works without it — until cost-lens pushes, cache state comes from nowhere and the F17 lines stay blank in daily use (question 8).

## 13. Live probes

The orchestrator runs the probes not marked as the owner's before section 6's setup PR, records each result as a Ruling and in `AGENTS.md`, and amends the named tasks before dispatch. **No task is blocked on an owner's probe:** each such task carries a built fixture, and its captured-fixture test skips, naming the probe, until the file exists.

**Method** (Ruling 33, and Plans 3 and 4): a private tmux server (`tmux -L curia-probe-<pid> -f /dev/null`, `TMUX_TMPDIR` in a short temp directory); Curia's state through a wrapper script that sets `HOME` and `CURIA_DB` to a temp directory before it runs the built binary; real `claude` only as `claude --settings <temp file> --setting-sources project`, with `claudeMdExcludes` for any stray instruction file above the probe directory (Plan 3's probe surprise 9), in a throwaway git repository under a temp directory, `--model haiku`. The real `~/.claude/settings.json`, `~/.tmux.conf`, `~/.config/curia`, `~/.codex` and the default tmux server are never touched. Known side effects, as before: transcripts land in the real `~/.claude/projects`. Throwaway files live in `.scratch/`; reports in `.superpowers/sdd/2026-11-13-curia-plan-5/`.

**Verified at the desk while writing this plan, and so not probes:** migration 0006's statements on SQLite 3.51.0 against stand-in tables; the cost-lens analyzer's location, command, run time (12 s) and output shape, and that cost-lens 0.3.0 contains no reference to Curia; `codex-cli` 0.166.0's `hooks` feature state, `--model`, `resume` and `mcp add` forms; the argument names of Agent Commons's `propose`; that the owner's `~/.claude/settings.json` has no `statusLine` key today.

| # | Who | Question | Experiment | If as assumed | Otherwise |
| --- | --- | --- | --- | --- | --- |
| S1 | orchestrator | What JSON does Claude Code give a `statusLine` command: the field names for the session id, cost, context and the two usage windows, the form of `resets_at`, whether `rate_limits` is present on a subscription and absent with an API key? How often is the command run, with what environment (`CLAUDECODE`, `TMUX_PANE`), and is its stderr shown? | A temp settings file whose `statusLine` command is a script that appends stdin and its environment (secret-looking names redacted) to a file and prints `ok`; two turns; then idle for a minute. | Section 4.3 stands; the payload becomes `statusline-captured.json`. | Other names or nesting: Task 2's mapping follows the capture. No `rate_limits`: the status-line source gives cost only, the forecast needs cost-lens, and README and decision 7 say so. Run more than once a second: Task 13's latency gate is restated per call and `SampleRepeat` already bounds the rows. |
| C2 | orchestrator | Does the cost-lens analyzer, given `--project <main repository>`, count sessions that ran in a sibling worktree (`<repo>/../.curia-worktrees/<repo>-T-1`) under that worktree's branch? | Spawn a task through Plan 3's `curia spawn` with real `claude` and one small turn; run `node <analyzer> --project <repo>` and `--project <worktree>`; compare `currentProject.branches`. Local files only. | Counted under the main repository: section 4.6 stands. | Counted only under the worktree path: `costlensRun` gets the worktree when it is on disk, else the repository (Task 19's argument amended), and a cleaned worktree's cost is "not recorded". Not counted at all: the branch cost is dropped from Task 19, the review keeps its session cost, and section 17 records the cut. |
| X2 | orchestrator | With `CODEX_HOME` set to a temp directory, what does `codex mcp add curia -- /x/curia mcp` write into `config.toml`, and what does `codex mcp remove curia` leave? Does either need a login? | Run both against the temp directory; copy the files. No model call. | A `[mcp_servers.curia]` table with `command` and `args`: Task 10 stands; the files become its fixtures. | Another shape: Task 10's recognition follows the capture. A login is required: X2 joins X1 as the owner's. |
| X1 | **owner must run or approve** (it starts a Codex session on the owner's OpenAI account and sends one prompt) | Where does Codex 0.166.0 read hooks from, which events exist, what does each payload hold (session id, cwd, transcript path, tool fields), does a hook see `TMUX_PANE` and `CURIA_TASK_ID`, what does Codex do with a Stop hook's stdout, and what does hook trust require? | In a temp `CODEX_HOME` copied from the real one only as far as login needs, a throwaway repository, a private tmux pane: declare a logging hook for every event, run one prompt that uses one tool, end the session. | Claude-compatible payloads and a file `hooks.json`: Tasks 8 and 10 stand; the payloads become fixtures. | Other field names: Task 8 gains a small Codex payload mapping in `internal/hook/parse.go` (one more file). A Stop block that works as Claude Code's: the Codex Stop claim is turned on (one condition in Task 8) and decision 27's cost goes away. No `TMUX_PANE`: Codex sessions are tracked without a pane (they show in `ls`, cannot be jumped to). Hooks need a trust step: `curia install --codex` prints it. |
| L1 | **owner must run** (it needs the owner's Linear API key and sends the query of section 4.9 to Linear) | Does Linear accept section 4.9's query, filter and `Authorization` form, and what does a real response look like? | `curl` the request with `CURIA_LINEAR_API_KEY` from the Keychain; save the response with titles and descriptions replaced by placeholders as `issues-captured.json`. | Task 5 and Task 16 stand. | A field or filter name differs: `linear.Query` and the decoder follow the capture (Task 5, or a fix PR if it has merged). The key needs a `Bearer` prefix: one line. |
| O1 | **owner must run** (it needs the owner's Obsidian) | Does a note built by `export.Note` from hostile text (an embed, a `dataviewjs` block, Templater tags, a front-matter look-alike, all inside fences) render inert in the owner's vault, and does the front matter read as properties? | `curia export obsidian` of one hand-made decision into a test folder of the vault; open it. | Decision 19 stands. | Something inside a fence is evaluated: that text is additionally indented as a quote block and the finding recorded (Task 4, or a fix PR). The properties do not parse: the quoting of `repo`, `task` and `tags` is changed to the form Obsidian reads. |
| A1 | **owner may run** | Does Agent Commons accept the printed entry when an agent passes it to `propose` with its own `task_id` and `model`? | `curia export commons <id>` for a real global decision; give the JSON to a session that has the commons tools. | Decision 22 stands. | A field is refused: `export.CommonsEntry`'s mapping is amended. |

Not probed, and why: the cost-lens push (it does not exist yet; question 8); the real status bar with a long badge (tmux cuts text at `status-right-length`, which Plan 1's live check already showed with the short badge; live check A looks at it again).

## 14. Deferred items: picked up, and cut from V1

Everything earlier plans, `PROGRESS.md` and the ledger marked for "Plan 5" or "a later plan" that no plan had taken.

**Picked up here**

| Item | From | Task |
| --- | --- | --- |
| `curia ingest`; real values for `cache_expires_at` and `cache_rebuild_usd` | Plan 2 section 15, decision 22; Plan 1's plan table | 2, 6, 13 |
| F17 with real values: queue order, the stakes line | Plan 1's plan table; Plan 2 decision 21 | 6 (`TestTicketsSeeIngestedCache`), live check A |
| The dashboard's cache and cost columns | Plan 2 decision 21 and section 15; Plan 1's plan table ("session cost column") | 20 |
| The stakes line in the ntfy push | Plan 4 section 16; spec F17 | 11 |
| Cost and cache state on a team's group row | Plan 3 decision 40, section 4.12 | 12, 20 |
| The cost in a review (`cost not recorded` until now); per-branch cost from the analyzer | Plan 4 decision 28, section 16 | 6 and 13 (the stored cost), 19 |
| Usage forecasting, task priority, budget-aware spawning | Plan 2 section 15; Plan 3 section 17 | 3, 14, 15, 17, 21 |
| The Linear adapter | Plan 3 decision 7, Open Question 6; Plan 4 section 16 | 5, 16 |
| Codex (`--agent codex`) | Plan 2 section 15; Plan 3 section 4.6; Plan 4 section 16 | 8, 10, 17 |
| Obsidian export | Plans 2, 3 and 4, out of scope | 4, 18 |
| Agent Commons | Plans 2, 3 and 4, out of scope | 4, 18 (printed; decision 22) |
| The note that a Stop-hook delivery is labelled "Stop hook error" | Plan 2 live check 2, defect 12 ("worth a line in the docs") | 23 |
| The upgrade note about an older long-lived `curia mcp` | Plan 2 re-review m6 ("say so in the upgrade text") | 23 |
| Whether `docs/DESIGN.md` should be updated to match the schema | Plan 2 Open Question 4; Plan 1 Open Question 20 | 23 (`docs/v1-as-built.md`; question 9) |

**Cut from V1, with the reason**

| Item | From | Why it is out |
| --- | --- | --- |
| Sending an Agent Commons entry over the network | spec F13 | Decision 22: a loose mapping, a second network client and a stored key; the entry is printed instead. |
| Typing into Codex, handing it answers at Stop, restoring an unlinked Codex session, Codex model and context in the dashboard | spec F13, Plan 4 section 16 | Decisions 27 and 29: each rests on Codex behaviour nobody has recorded; `check_decision` works today. |
| The cost-lens push and a Linux cache path in cost-lens | spec F15 | Another repository; the contract and the receiving command are here (decision 36). |
| cost-lens's model advisor for the spawn default | spec F16 | It has no interface outside the mod (decision 15). |
| Token deltas for attribution | spec F16 | Decision 12. |
| `curia forget`, or an age-out, for a session outside tmux that died without SessionEnd | Plan 1 Open Question 12 ("a later plan") | Not a spec feature; rare (a session outside tmux, killed hard, while waiting); the row can be ended by its next event. First item of the after-V1 list. |
| A liveness check of the pane's foreground process (a SIGKILLed Claude stays `working` or `idle`) | Plan 1 live-check defect 15; Plan 2 Open Question 17 | The owner accepted typing without it; the pane check refuses to type into a shell. |
| Uninstalling entries of a binary that moved; `#` or `%` in the binary path | Plan 1 Open Questions 8, 14, 16 | Ruling 104 records the stable path, which removes the common cause; the rest is packaging polish. |
| `synchronous=NORMAL` on Linux | Plan 1 Open Question 15 | Unmeasured, and no Linux user yet. |
| CI printing latency numbers | Plan 2 Open Question 19 | It needs a change to the shared check; the gates still fail when a latency creeps. |
| The closing "Continue." of a typed answer; a second chance at the next Stop for an unconfirmed typed line; `last_update` staying `interrupted`; Escape before a turn's first output | Plan 2 deferred minors; Plan 4 section 16 | Wording and edge cases of the typing path, which Plan 5 does not touch; they belong to a fix release with a live check of their own. |
| A status-line hint for tickets with no session | Plan 2 Task 16 minor | `curia decide` lists them; no report of one being missed. |
| The decide view's `face` and the stakes line | Plan 2 Task 10 minor | Decision 34. |
| The operator skill | spec F14 | The spec defers it (decision 35). |
| A per-repository switch to relax an agent's permissions; Claude Code's native worktrees | spec section 7 and F6 | Plan 3 decisions 45 and 1; unchanged. |
| The per-task minors listed in `PROGRESS.md` | Plans 1 to 4 | Ledgered; none blocks a V1 goal. They move to the after-V1 list in Task 23. |

## 15. Questions only the owner can answer

Each has the default this plan builds and blocks nothing.

1. **`curia install` adds a status line to Claude Code when you have none** (decision 7). It prints the model, context, cost and five-hour usage, and it is the only live source of usage numbers until cost-lens pushes. Default: added, `--no-statusline` to skip, never replacing yours.
2. **Pausing on a forecast takes `P` and then `y`,** not the spec's one key (decision 13). Default: two keys.
3. **The cheaper model is `sonnet`** (`usage.cheaper_model`). Default: `sonnet`; say so if it should be another name or off.
4. **Agent Commons: Curia prints the entry and does not send it** (decision 22). Default: printed. Say so if a direct publish is worth a task after V1, and whether a decision is the kind of entry you want in that store at all.
5. **Obsidian notes put all ticket text and your reasoning inside code fences** (decision 19), in the folder `obsidian.dir`, written after each `curia decide`. Default: as planned. Probe O1 is yours.
6. **Linear: which issues.** Default: assigned to you, backlog or unstarted, optionally one team, at most 200; the key in `CURIA_LINEAR_API_KEY` (a Keychain item `env:CURIA_LINEAR_API_KEY`). Probe L1 is yours.
7. **Codex: approve probe X1** (one Codex session on your OpenAI account, in a throwaway repository). Default: not run; Codex is tracked and spawned on built fixtures, and `curia install --codex` waits.
8. **Who builds the cost-lens push,** and when. Default: you, in cost-lens, after V1, against `docs/ingest-schema.json`; until then cache state is blank in daily use and the forecast runs on the status line's numbers.
9. **`docs/DESIGN.md` against what was built.** Default: the spec stays as you wrote it and `docs/v1-as-built.md` lists every difference. Say so if the spec itself should be amended.
10. **The tag `v1.0.0`.** Default: the orchestrator prepares the changelog and stops; you push the tag, which publishes the release and the Homebrew formula.
11. **Attribution in dollars** (decision 12) and **no API-mode figure in the badge** (decision 11). Default: as planned.
12. **Plan 2 Open Question 22, still open:** typing depends on recognizing Claude Code's screen, so a Claude Code upgrade can turn typing off silently. Default: after an upgrade the orchestrator re-captures the idle screens in a live check when asked; V1 does not check the Claude Code version by itself.

## 16. Out of scope

Not planned, in V1 or after it unless you ask: a price table or any cost computation in Curia (decision 18); writing anything back to Linear (the spec's non-goal); a two-way Obsidian sync, or reading notes back; importing from Obsidian; pushing usage warnings to the phone; pausing, killing or changing a model without a key (section 2); a daemon that samples usage on a timer; a `curia install` step for cost-lens; restoring pane layouts (Plan 4); sharing the store between machines or users (the spec's non-goal).

After V1, in the order this plan would take them: `curia forget`; the direct Agent Commons publish, if question 4 says so; Codex delivery at Stop and Codex restore, once probe X1 has shown what Codex does; the typing path's wording minors with a live check; the board showing priority; the operator skill.

## 17. V1 completion

This is the last plan of V1. The tables below are what Task 23 copies into `PROGRESS.md` with each row's final state.

**The spec's goals for V1 (section 2), one by one**

| # | Goal | Satisfied by | How it is checked |
| --- | --- | --- | --- |
| 1 | Any agent needing me is visible within 2 seconds, from any tmux window, without visiting panes | Plan 1 Tasks 5 (hook), 7 (`status`, `next`), 8 (window markers), 11 (latency); Plan 2 Tasks 16 (`D`, `Q`) and 19 (settling a denied or interrupted turn); Plan 4 Tasks 10 and 21 (`R`, the blue marker); Plan 5 Task 14 (the limit warning) | `TestHookLatency` (p95 under 20 ms), the replay test (a session appears within a second), the 2 second `status-interval`; live checks of Plans 1 and 2. Known gap: a permission prompt shows up to 6 seconds late (Ruling 48). |
| 2 | A decision can be answered in under 30 seconds from a tmux popup, or from a phone over SSH | Plan 2 Tasks 10 and 14 (`curia decide`, `TestDecideViaSSH`, `TestDecideNarrowWidth`); Plan 4 Task 19 (the ntfy button) | The acceptance tests of Plans 2 and 4. The 30 seconds and the phone's width are the owner's to confirm (Plan 4 question 6). |
| 3 | Every decision is stored with its reasoning and is searchable by agents before they raise a new one | Plan 2 Tasks 9, 11, 12 (the store, FTS search, the two checkpoints); Plan 3 Task 13 (standing rules as precedent); Plan 5 Task 18 (the Obsidian copy) | `TestRequestReturnsStrongPrecedent`, `TestSearchScopeTable`, Plan 2's acceptance test; Plan 5's step 8. |
| 4 | A task goes from backlog to running agent in one command, with an isolated worktree and branch | Plan 3 Tasks 16 and 19 (`curia task`, `curia spawn`); Plan 5 Tasks 16 (Linear as a source) and 17 (`--agent codex`, the model default) | Plan 3's acceptance test; Plan 5's steps 2, 6 and 10. |
| 5 | Finished work lands in a review queue with diff, decisions, open assumptions and cost in one view | Plan 4 Tasks 6, 8, 12, 16; the cost: Plan 5 Tasks 6 and 13 (it is stored at last) and 19 (the branch cost) | Plan 4's acceptance test; Plan 5's step 11. Without a cost source the line reads `cost not recorded`. |
| 6 | Works headless, over SSH, and survives laptop sleep or disconnect | Plan 1 (no daemon, one SQLite store, every view reads it); Plan 2 (`decide` over SSH, the Stop-hook delivery that needs no attached client); Plan 4 Task 18 (`curia restore`) | Every acceptance test runs headless on a detached tmux server; Plan 4's step 8. Sleep itself is not tested: nothing in Curia holds a connection that sleep could break, except `curia serve`, which reconnects by being stateless. |
| 7 | Agent-agnostic where hooks allow: Claude Code first, Codex second | Plan 5 Tasks 8 (the hook), 10 (install), 17 (spawn) | Plan 5's step 10. Partial by design: Codex is tracked, spawned and can use the MCP tools; it is not typed into, not handed answers at Stop and not restored when unlinked (decisions 27 to 29), and Task 10 waits for the owner's probe. |

**The features, and what of each is not in V1**

| Feature | Plan | In V1 | Not in V1, and where that was decided |
| --- | --- | --- | --- |
| F1 Session tracking | 1, 2 | all | SessionStart is `idle`, not `working` (Ruling 37) |
| F2 Session metrics | 1, 5 | model, context, subagents; cost from ingest | no price table (by the spec) |
| F3 Dashboard | 1, 2, 3, 4, 5 | all columns and keys; sidebar | |
| F4 Attention signals | 1, 2, 4 | badge, markers, jump, bell | badge format amended (Ruling 91) |
| F5 Task board | 3, 5 | local, Backlog.md and Linear sources; kanban | priority is not on the board (decision 16) |
| F6 Spawning and the agent CLI | 3, 5 | spawn, pause, resume, clean, `ls`, `read`, `send`; Codex spawn | Claude Code's native worktrees (Plan 3 decision 1); the per-repository permission switch (Plan 3 decision 45) |
| F7 Decision queue | 2 | all | |
| F8 Assumptions | 2, 4 | all | |
| F9 Precedent | 2, 3 | all | embeddings (spec section 9: decide after about 50 real decisions) |
| F10 Review queue | 4, 5 | all | a pull request's later merge is not tracked (Plan 4 decision 9) |
| F11 Notifications and remote | 4, 5 | pushes, quiet hours, `curia serve`, the stakes line | a long-lived per-device token (Plan 4 decision 16); approving a review from the phone (Plan 4 decision 18) |
| F12 Restore | 4 | all | pane layouts (Plan 4 decision 24) |
| F13 Integrations | 5 | Obsidian, Linear, the cost-lens analyzer, Codex tracking and spawn | sending to Agent Commons (decision 22); the rest of Codex (decisions 27 to 29) |
| F14 Agent Teams | 3, 4, 5 | detection, grouping, escalation, `--team`, cost and cache on the group row and in the review | the operator skill (deferred by the spec) |
| F15 cost-lens integration | 5 | `curia ingest`, the schema file, the status-line source, the analyzer pull | the push inside cost-lens and its Linux cache path (another repository; question 8) |
| F16 Forecasting and scheduling | 5 | samples, burn, the badge warning, API projection, attribution, priority, the pause suggestion, the cheaper default | the model advisor (decision 15); token deltas (decision 12); one-key accept (decision 13) |
| F17 Cache-aware triage | 2, 5 | order, the stakes line in the popup and the push, the cache column | in daily use it shows values only once cost-lens pushes (decision 36) |
| F18 Message routing | 3 | all | |

**Spec section 9's open questions, as V1 leaves them:** intercepting Claude Code's own ask-the-user tool is not attempted; FTS5 against embeddings waits for real decisions; the assume expiry is 24 hours and also ends when a task's review is approved (Plan 4 decision 8); the licence is MIT; Stop-hook delivery works with a background subagent running (Plan 2's probe) and a long handed-over text can be refused by the model (Ruling 100); the sidebar exists and its worth is the owner's to judge.

**What the owner still has to do for V1 to be true on their machine:** run `curia install` on the real machine and the three-session check (open since Plan 1); the phone checks of Plan 4 (question 6 there); probes X1, L1 and O1 of this plan; build or schedule the cost-lens push (question 8); push the tag `v1.0.0` (question 10).

## Appendix A: the interface and file check

The check the plan was held to before it was finished. Two tables: where every consumed interface comes from, and which files each wave touches. "Wave" is the earliest point at which a task can start (section 7); a task consumes only what an earlier wave merged, or what `main` holds after Plans 3 and 4.

**A.1 Consumed interfaces.** "main" means the code at 33db0e4 (file given); "P3" and "P4" mean those plans' documents, to be re-verified per the dependency table (row given).

| Task (wave) | Consumes | Produced by |
| --- | --- | --- |
| 1 (1) | `model.Session`, `Event`, `Decision`; `config.Config`, `Load`, `expandHome` | main, `internal/model/model.go`, `decision.go`, `internal/config/config.go` |
| | `model.Task`, `TaskSource` | P3 Task 1 (E3) |
| 4 (1) | `model.Decision`, `Option`; `decision.IsDecided`; `sanitize.Line`, `Text` | main, `internal/model/decision.go`, `internal/decision/resolve.go`, `internal/sanitize/sanitize.go` |
| 5 (1) | `sanitize.Line` | main |
| 9 (1) | `loadSettings`, `saveSettings`, `shellQuote`; `executablePath`, `installSay`, `runInstall`, `runUninstall` | main, `internal/install/settings.go`, `cmd/curia/install.go` (E14) |
| 11 (1) | `notify.DecisionPush`; `pushFlush` | P4 Tasks 7, 15 (E12) |
| | `decision.Stakes`, `store.OpenTickets` | main, `internal/decision/triage.go`, `internal/store/decisions.go` |
| 2 (2) | `model.UsageWindow` and its values | Task 1 (wave 1) |
| | `sanitize.Line` | main |
| 3 (2) | `model.UsageSample`, `CostSample`, the session's cost and cache fields | Task 1 (wave 1) |
| 6 (2) | `model.UsageSample`, `CostSample`, the session fields | Task 1 (wave 1) |
| | `begin`, `formatTime`, `parseTime`, `upsert`, `selectSessions`, `scanSession` | main, `internal/store/store.go`, `decisions.go` (E2) |
| | migrations 0004, 0005 | P3 Task 8, P4 Task 6 (E1) |
| 8 (2) | `model.Event.Agent`, `AgentClaude`, `AgentCodex` | Task 1 (wave 1) |
| | `state.Apply`; `hook.Env`, `Run`, `runStop`, `readTranscript`; `runHook` | main, `internal/state/state.go`, `internal/hook/run.go`, `cmd/curia/hook.go`; P3 Tasks 14, 18, P4 Task 13 (E15) |
| 10 (2) | `cmd/curia/install.go` as Task 9 leaves it | Task 9 (wave 1) |
| | `hookEvents`, `withoutOurs`, `containsOurs`, `writeBackup`, `writeAtomic`, `shellQuote`; `Runner`, `MCPState` | main, `internal/install/settings.go`, `mcp.go` |
| 12 (2) | the session's cost and cache fields | Task 1 (wave 1) |
| | `fleet.Counts`, `Badge`; `Unit` | main, `internal/fleet/fleet.go`; P3 Tasks 9, 29 (E9) |
| 7 (3) | `Task.Priority`, `Task.Agent`, `Decision.ExportedAt` | Task 1 (wave 1) |
| | the columns of 0006 | Task 6 (wave 2) |
| | the task statements; `selectDecisions`, `scanDecision`, `ErrNotFound` | P3 Task 8 (E4); main, `internal/store/decisions.go` |
| 13 (3) | `usage.ParseDoc`, `FromStatusLine`, `StatusText`, `Doc`, `MaxDoc` | Task 2 (wave 2) |
| | `store.Ingest`, `Reading` | Task 6 (wave 2) |
| | `loadConfig`, `openStore`, `openLog`, `clock`; the hook's SIGPIPE and watchdog pattern | main, `cmd/curia/common.go`, `hook.go` |
| 14 (3) | `usage.Forecasts`, `Warning`, `BadgeText`, `Line`, `Burns`, `APIProjection`, `SuggestPause`, `Running`, `CostCell`, `BurnWindow` | Task 3 (wave 2) |
| | `store.UsageSince`, `CostSince` | Task 6 (wave 2) |
| | `fleet.Counts.Forecast` | Task 12 (wave 2) |
| | `statusBadge`, `listSettled`; `ui.Cell` | main, `cmd/curia/status.go`, `common.go`, `internal/ui/view.go` |
| | `taskCards` | P3 Task 16 (E6) |
| 19 (3) | `usage.BranchCost` | Task 2 (wave 2) |
| | `config.CostLens` | Task 1 (wave 1) |
| | `ui.ReviewDetail`, the review view, the review source, `review --json` | P4 Tasks 8, 16 (E11) |
| 20 (3) | `usage.CostCell`, `CacheCell`, `Burns`, `Forecasts`, `Warning`, `BadgeText`, `APIProjection`, `BurnWindow` | Task 3 (wave 2) |
| | `store.UsageSince`, `CostSince` | Task 6 (wave 2) |
| | `Unit.Cost`, `Unit.CacheOf` | Task 12 (wave 2) |
| | `ui.Model`, `Source`, the columns and widths, `storeSource` | main, `internal/ui/model.go`, `view.go`, `cmd/curia/ui.go`; P3 Task 30, P4 Task 20 (E10) |
| 15 (4) | the store's priority and agent | Task 7 (wave 3) |
| | `task.Normalize`, `Validate`, `Problem`; `curia task add`, `edit`, `ls`, the golden file | P3 Tasks 6, 16 (E5, E6) |
| 16 (4) | `linear.Client`, `Issue`, `ValidKey`, `ValidTeam` | Task 5 (wave 1) |
| | `store.ImportTask` with priority | Task 7 (wave 3); P3 Task 8 (E4) |
| | `config.Linear`, `model.SourceLinear` | Task 1 (wave 1) |
| | `task.Normalize`, `Validate`, `MaxBody`; the import command and its repository resolution | P3 Tasks 6, 16 (E5, E6) |
| 17 (4) | `usage.Forecasts`, `Warning`, `BadgeText`, `BurnWindow` | Task 3 (wave 2) |
| | `store.UsageSince` | Task 6 (wave 2) |
| | the stored `tasks.agent` | Task 7 (wave 3) |
| | `config.Usage`, `model.AgentCodex` | Task 1 (wave 1) |
| | `spawnOptions`, `spawnTask`, `spawnResult`, `spawnLine`, `launchExec`, `launchLookPath`; `task.ValidModel`, `Brief` | P3 Tasks 19, 6 (E7, E5) |
| 18 (4) | `export.NoteName`, `IsNoteOf`, `OwnsNote`, `Note`, `CommonsEntry`, `ErrNotPublishable` | Task 4 (wave 1) |
| | `store.ExportQueue`, `MarkExported` | Task 7 (wave 3) |
| | `config.Obsidian` | Task 1 (wave 1) |
| | `store.GetDecision`; `decision.ValidID`; the answer paths of `cmd/curia/decide.go` | main (E13) |
| 21 (4) | `ui.Budget`, `Source.Budget`, the files as Task 20 leaves them | Task 20 (wave 3) |
| | `usageBuild`, `usageReport` | Task 14 (wave 3) |
| | task priority in the cards | Task 7 (wave 3) |
| | the `x` confirm prompt and dwell; `refuseInsideAgent` | main, `internal/ui/model.go`, `cmd/curia/decisions.go` |
| | `curia pause` | P3 Task 24 (E8) |
| 22 (5) | the built binary's commands | Tasks 8 to 21 (waves 1 to 4); P3; P4 |
| | the stub `claude`, the e2e helpers, the stand-in ntfy | P3 Task 31, P4 Task 22 (E16); main, `e2e/` |

No task consumes something produced in its own wave or a later one. Checked a second time for types and constants alone, which is where Plan 3 slipped: every type and constant a task names is in `main`, in Plans 3 or 4, or created by a task of an earlier wave. The ones that would have crossed a wave were placed early: `model.AgentCodex`, `SourceLinear`, `Event.Agent`, `Task.Priority` and every config section live in Task 1, so Tasks 8, 16 and 17 share them without touching `internal/model`; `usage.BurnWindow` (Task 3, wave 2) is first used in wave 3; `store.Reading` (Task 6, wave 2) is first used in wave 3; `ui.Budget` (Task 20, wave 3) is extended only by Task 21 (wave 4), which is the next and only other editor of that file; `usageBuild` (Task 14, wave 3) is first reused in wave 4. Task 6 takes a `store.Reading` and not a `usage.Doc`, so Tasks 2 and 6, both wave 2, do not depend on each other.

**A.2 Files per wave.** A file appears at most once in a row.

| Wave | Files created or modified (task) |
| --- | --- |
| 1 | `internal/model/usage.go`, `model.go`, `task.go`, `decision.go`, `internal/config/config.go`, `config_test.go` (1); `internal/export/obsidian.go`, `commons.go`, `export_test.go` (4); `internal/linear/linear.go`, `linear_test.go`, `testdata/` (5); `internal/install/statusline.go`, `statusline_test.go`, `cmd/curia/install.go`, `install_test.go` (9); `internal/notify/ntfy.go`, `ntfy_test.go`, `cmd/curia/push.go`, `push_test.go` (11) |
| 2 | `internal/usage/ingest.go`, `ingest_test.go`, `costlens.go`, `costlens_test.go`, `testdata/` (2); `internal/usage/forecast.go`, `forecast_test.go` (3); `internal/store/migrations/0006_cost.sql`, `usage.go`, `usage_test.go`, `store.go`, `store_test.go` (6); `internal/state/state.go`, `state_test.go`, `internal/hook/run.go`, `hook_test.go`, `testdata/codex/`, `cmd/curia/hook.go`, `hook_test.go` (8); `internal/install/codex.go`, `codex_test.go`, `testdata/codex/`, `cmd/curia/install.go`, `install_test.go` (10); `internal/fleet/fleet.go`, `fleet_test.go`, `units.go`, `units_test.go` (12) |
| 3 | `internal/store/tasks.go`, `tasks_test.go`, `decisions.go`, `decisions_test.go` (7); `cmd/curia/ingest.go`, `ingest_test.go`, `statusline.go`, `statusline_test.go` (13); `cmd/curia/status.go`, `status_test.go`, `usage.go`, `usage_test.go` (14); `cmd/curia/costlens.go`, `costlens_test.go`, `review.go`, `review_test.go`, `internal/ui/review.go`, `review_view.go`, `review_test.go` (19); `internal/ui/model.go`, `view.go`, `ui_test.go`, `cmd/curia/ui.go`, `ui_test.go` (20) |
| 4 | `internal/task/task.go`, `task_test.go`, `cmd/curia/task.go`, `task_test.go`, `cmd/curia/testdata/tasks.golden.json` (15); `cmd/curia/task_import.go`, `task_import_test.go` (16); `cmd/curia/spawn.go`, `spawn_test.go`, `launch.go`, `launch_test.go` (17); `cmd/curia/export.go`, `export_test.go`, `decide.go`, `decide_test.go` (18); `internal/ui/model.go`, `view.go`, `ui_test.go`, `cmd/curia/ui.go`, `ui_test.go`, `pause.go` (21) |
| 5 | `testdata/cost/`, `e2e/cost_test.go`, `e2e/testdata/cost/` (22) |
| after | `README.md`, `CHANGELOG.md`, `AGENTS.md`, `PROGRESS.md`, `docs/v1-as-built.md` (23, orchestrator) |

Checked by hand for the places where names are close or a file has two editors: `cmd/curia/install.go` and `install_test.go` are Task 9's in wave 1 and Task 10's in wave 2, in that order; `internal/ui/model.go`, `view.go`, `ui_test.go` and `cmd/curia/ui.go`, `ui_test.go` are Task 20's in wave 3 and Task 21's in wave 4, in that order; `internal/ui/review_test.go` (Task 19) and `cmd/curia/review_test.go` (Task 19) are one task's; `internal/usage/costlens.go` (Task 2: the pure reader) and `cmd/curia/costlens.go` (Task 19: the runner) are in different directories and waves; `cmd/curia/usage.go` (Task 14: the command) and `internal/store/usage.go` (Task 6) and `internal/model/usage.go` (Task 1) are three files in three packages; `internal/hook/testdata/codex/` (Task 8) and `internal/install/testdata/codex/` (Task 10, from setup) are different directories; `cmd/curia/task.go` (Task 15) and `cmd/curia/task_import.go` (Task 16) are different files in one wave; `cmd/curia/decide.go` is edited by Task 18 alone and `cmd/curia/push.go` by Task 11 alone.
