# PROGRESS

This file mirrors the plans for humans; it does not replace them. Task order, interfaces and tests come from the current plan, `docs/superpowers/plans/2026-11-11-curia-plan-2-decisions.md`. Plan 1 (`docs/superpowers/plans/2026-11-11-curia-plan-1-foundation.md`) is complete; its record is kept below.

# Plan 2 (Decisions): in progress

## Plan 2 status

- **Setup in review.** `chore/plan2-setup` holds the plan document (with the pre-flight review's changes applied), the `cmd/curia` test guard of plan section 6.8, and every shared-file change of plan section 6: depguard rules for the six new packages (decision 0010), the layout check's package list, CI on pushes to `plan-2`, AGENTS.md, decision records 0010 to 0014, and this file. Each boundary was proven with temporary probes (decision 0010, Consequences). Its PR goes into `main` (rebased onto the live-check fixes, PR #15).
- The three pre-wave-1 live checks are done (2026-11-11, Claude Code 2.1.322, tmux 3.8): plan decisions 36 to 41, Rulings 42 to 47.
- Integration branch: `plan-2`, to be branched from `main` once the setup PR merges. Every task PR goes into `plan-2`.
- No task has been dispatched.

## Plan 2 waves

Every task's done criteria also include: `make check` passes in the task's worktree; the PR into `plan-2` uses the PR template; CI (`check`) is green; the task review is clean or every finding is parked with a ruling; no shared file is touched (plan section 2).

Model column: the plan's expected models (section 7), **expected, confirmed at dispatch**. The orchestrator confirms or changes it, with a one-line reason, before dispatching.

### Before wave 1

| Step | Owner | Done criteria | Status |
| --- | --- | --- | --- |
| Live checks | orchestrator | the three checks of plan section 14 (environment of a stdio MCP server, Enter after typed text, `claude mcp get` output) run in the isolated setup; results recorded as Plan 2 Rulings; T3, T5 and T12 amended before dispatch | done 2026-11-11 (Claude Code 2.1.322, tmux 3.8); plan decisions 36 to 41; T3, T4, T5, T12, T13, T14, T17 amended |

Dependencies (plan section 7): 6 and 7 need 1 and 2; 8 needs 1, 2, 3; 9 needs 2, 6; 10 needs 1, 2, 6; 11 needs 7, 9; 15 needs 9; 12 needs 4, 6, 7, 8, 11; 13 needs 6, 11; 14 needs 3, 6, 10, 11; 16 needs 9, 10, 14; 17 needs everything.

### Wave 1

| Task | Title | Model (expected, confirmed at dispatch) | Done criteria | Status |
| --- | --- | --- | --- | --- |
| 1 | Sanitiser | Sonnet (expected, confirmed at dispatch) | `sanitize.Line`, `Text`, `Keys` (with `lossy`), one rune scanner, no regular expressions; `TestLineStripsEscapeSequences`, `TestLineOneLine`, `TestLineDropsInvisible`, `TestLineLimit`, `TestTextKeepsParagraphs`, `TestKeysAlphabet` (and `Keys` re-collapses after its filter and cut), `TestKeysLimitIsLossy`, `TestIdempotent`, `TestHostileCorpus` pass under `-race`. | merged (PR #19), Sonnet, 1 fix rounds |
| 2 | Decision types and config | Sonnet (expected, confirmed at dispatch) | `internal/model/decision.go` types and enums exactly as the Interfaces block (types only); `model.Event.StopBlocked`; `config.AssumeExpiry` (default 24h), `NotifyBell`, `NotifyMacOS`; `TestDecisionDefaults`, `TestLoadDecisionOverrides`, `TestNonPositiveAssumeExpiryRejected` pass; `TestDefaults` gains the three assertions; `go build ./...` works. | merged (PR #17), Sonnet, 0 fix rounds |
| 3 | tmux: typing and bell | Opus (expected, confirmed at dispatch) | `tmux.Client.SendKeys` and `Bell` on the exec client and `tmux.Fake` (with `SendKeysErr`); input guard independent of `sanitize`, `copy-mode -q`, `send-keys -l --` and `send-keys Enter` as three separate tmux invocations, at most 800 runes and never a single rune (plan decision 38), `Bell` checks the pane id; trailing `;` sent as `\;`; `TestSendKeysRefusesBadInput`, `TestSendKeysLiteralInRealTmux`, `TestSendKeysUnknownPaneFails`, `TestSendKeysLeavesCopyMode`, `TestSendKeysIsThreeSeparateInvocations`, `TestSendKeysAcceptsEightHundredRunes`, `TestBellInRealTmux`, `TestBellUnknownPaneFails`, `TestFakeRecordsSendKeysAndBell` pass, the real-tmux ones not skipped where tmux is installed. | merged (PR #20), Opus, 2 fix rounds |
| 4 | MCP protocol server | Sonnet (expected, confirmed at dispatch) | `mcp.Server.Serve` over line-delimited JSON-RPC 2.0 with the plan's five methods and error codes, `-32601` at once for any unknown request (Claude Code opens with `server/discover`), `_meta` ignored, standard library only (decision 0011, plan decision 37); `TestInitializeNegotiatesVersion`, `TestToolsListInOrder`, `TestToolCallResultAndError`, `TestUnknownToolIsInvalidParams`, `TestProtocolErrors`, `TestNotificationsGetNoReply`, `TestOversizedLineSkipped`, `TestPanicInToolIsContained`, `TestOutputIsOneLinePerMessage`, `TestEOFEndsServe`, `TestDiscoverThenInitialize`, `TestToolCallWithMeta`, `TestResponseMessageIgnored` pass through real pipes; a message with an `id` and no `method` gets no reply. | merged (PR #18), Sonnet, 1 fix rounds |
| 5 | Install additions | Opus (expected, confirmed at dispatch) | `install.WriteSkill`/`RemoveSkill` with the embedded `skill/SKILL.md` (exact content), `AddMCP`/`RemoveMCP` through a `Runner`, recognising the entry by reading the Claude config JSON (`ClaudeConfigPath`) read-only and never running `claude mcp get` or `list` (plan decision 40), `AllowTools` for the four exact tool names only with `curia install --allow-mcp-tools`, `Unhooks` removing them, `ErrForeignSkill`, the order and failure rules, the `A` binding; `claudeRunner` defaults to a failing stub; `TestWriteSkillCreatesFile`, `TestWriteSkillIdempotentAndUpgrades`, `TestWriteSkillLeavesForeignFile`, `TestRemoveSkillOnlyOurs`, `TestRemoveSkillKeepsOtherFilesInDir`, `TestSkillSymlinkedDirSurvives`, `TestAddMCPSequence`, `TestAddMCPAlreadyOurs`, `TestAddMCPForeign`, `TestAddMCPNoClaude`, `TestRemoveMCPOnlyOurs`, `TestBinWithSpacesIsOneArgument`, `TestNeverRunsMCPGetOrList`, `TestConfigJSONNeverWritten`, `TestClaudeConfigPath`, `TestHooksAddNoPermissionRule`, `TestAllowToolsAddsFourOnce`, `TestUnhooksRemovesTheFourToolRules`, `TestPermissionsOfWrongTypeUntouched`, `TestInstallWritesSkillAndRegistersMCP`, `TestInstallThroughStubClaude`, `TestUninstallReversesPlan2Items`, `TestInstallAllowsToolsOnlyWithFlag`, `TestInstallForeignSkillContinues` pass; the real-tmux binding loop checks `A`; no test runs the real `claude`. | merged (PR #21), Opus, 1 fix rounds |

### Wave 2

| Task | Title | Model (expected, confirmed at dispatch) | Done criteria | Status |
| --- | --- | --- | --- | --- |
| 6 | Ticket rules | Opus (expected, confirmed at dispatch) | `decision.Parse`, `Normalize`, `Validate`, `ArgsHash`, `New`, `ValidID`, `IsDecided`, `Resolve`, `Triage`, `Stakes`, `TypedMessage`, `StopReason`, all pure, limits as exported constants; `TestValidateAcceptsGoodTicket`, `TestValidateReportsEveryProblem`, `TestNormalizeCleansAndSortsTags`, `TestDefaultScopeIsRepo`, `TestParseTypeErrorIsOneProblem`, `TestArgsHashVector`, `TestArgsHashIgnoresDiffersAndTagOrder`, `TestArgsHashChangesWithMeaning`, `TestNewSetsExpiryForAssumeOnly`, `TestValidID`, `TestIsDecided`, `TestResolveTable`, `TestResolveRejectsBadAnswers`, `TestTriageOrder`, `TestTriageAllUnknownIsOldestBlockingFirst`, `TestStakes`, `TestTypedMessageWording`, `TestTypedMessageIsInert`, `TestStopReasonJoinsAndCaps` pass. | merged (PR #24), Opus, 1 fix rounds |
| 7 | Precedent matching | Sonnet (expected, confirmed at dispatch) | `precedent.Tokens`, `MatchExpr`, `Similarity`, `Strong` and the constants, pure; `TestTokens`, `TestMatchExprQuotesAndCaps`, `TestMatchExprHostile`, `TestMatchExprEmpty`, `TestSimilarity`, `TestStrongThreshold`, `TestStrongCapsAtThree` pass. | merged (PR #22), Sonnet, 1 fix rounds |
| 8 | Notifications | Sonnet (expected, confirmed at dispatch) | `notify.Run`, `notify.Exec`, `notify.Blocking` (bell and the fixed `osascript` script with ticket text only as an argument); `TestBlockingOnlyForBlockingTickets`, `TestBellWhenEnabledAndInPane`, `TestMacOSPassesTextAsArguments`, `TestMacOSOnlyOnDarwin`, `TestBothTriedWhenOneFails`, `TestBodyCappedAt140`, `TestExecRunsAProgram` pass; no test runs `osascript`. | merged (PR #23), Sonnet, 1 fix rounds |

### Wave 3

| Task | Title | Model (expected, confirmed at dispatch) | Done criteria | Status |
| --- | --- | --- | --- | --- |
| 9 | Store: decisions | Opus (expected, confirmed at dispatch) | `0002_decisions.sql` exactly as plan section 4.1; `CreateDecision`, `GetDecision`, `ListDecisions`, `OpenTickets`, `OpenCounts`, `CountOpenBlocking`, `AnswerDecision`, `WithdrawDecision`, `ExpireDecisions`, `EditDecision`, each write one `BEGIN IMMEDIATE` transaction (expiry one `UPDATE`), `CreateDecision` expiring a due twin first; `TestOpenIsIdempotent` and `TestConcurrentOpenOfFreshFile` expect 2 migrations; `TestMigrationAddsDecisions`, `TestMigrationOnPlan1Database`, `TestCreateAllocatesSequentialIDs`, `TestDecisionRoundTrip`, `TestIdenticalRetryCollapses`, `TestRetryAddsDiffers`, `TestRetryAfterDecisionReturnsIt`, `TestRetryAfterWithdrawOrExpiryOpensNew`, `TestRetryPastDeadlineBeforeAnySweepOpensNew`, `TestSameTicketFromTwoSessionsIsTwoRows`, `TestConcurrentCreatesAllocateDistinctIDs`, `TestConcurrentIdenticalRetriesCollapse`, `TestAnswerTable`, `TestAnswerErrors`, `TestConcurrentAnswersOneWins`, `TestAnswerRacesWithdrawAndExpire`, `TestAnswerClearsNeedsDecision`, `TestAnswerKeepsNeedsDecisionWhileAnotherIsOpen`, `TestAnswerDoesNotTouchOtherStates`, `TestWithdrawRules`, `TestPastDeadlineIsNotOpenBeforeTheSweep`, `TestExpireFlipsOnlyDueAssume`, `TestEditRescopeAndSupersede`, `TestOpenTicketsJoinsSession`, `TestOpenCountsAndBlockingCount`, `TestListDecisionsFilters`, `TestFTSStaysInSync`, `TestCacheColumnsSurviveSessionUpsert` pass under `-race`, with all Plan 1 store tests. | merged (PR #26), Opus, 1 fix rounds |
| 10 | Decide view | Opus (expected, confirmed at dispatch) | `ui.DecideSource`, `DecideOptions`, `DecideClosedMsg`, `NewDecide`; every ticket string through `sanitize`; an answer key on a blocking ticket with nothing staged opens the reasoning input; the selection follows the ticket id; `TestDecideListsTicketsInSourceOrder`, `TestDecideShowsSelectedTicket`, `TestNumberKeyAnswers`, `TestNumberBeyondOptionsIgnored`, `TestEnterAcceptsRecommendation`, `TestFreeTextAnswer`, `TestStagedReasoningAndScopeTravel`, `TestMovingDropsStaged`, `TestIrreversibleNeedsConfirmation`, `TestBlockingAnswerPromptsForReasoning`, `TestRefreshNeverRedirectsAnAnswer`, `TestSelectionFollowsTicketAcrossReorder`, `TestOutcomeShownAndQueueRefreshed`, `TestAnswerErrorShown`, `TestSessionFilter`, `TestEmbeddedCloseSendsMessage`, `TestEmptyQueue`, `TestTicketTextCannotDrawEscapes`, `TestDecideNarrowWidth`, `TestPasteIsNotACommand`, `TestInputCaps` pass. | merged (PR #25), Opus (round 4: Fable), 4 fix rounds |

### Wave 4

| Task | Title | Model (expected, confirmed at dispatch) | Done criteria | Status |
| --- | --- | --- | --- | --- |
| 11 | Store: search, delivery and the hook's transaction | Opus (expected, confirmed at dispatch) | `SearchDecisions` (via `precedent.MatchExpr`, bound parameters), `ClaimTyping`, `RequeueDelivery`, `CheckDecision`, `UpdateSessionTx` with `Tx.ClaimDeliveries` and `Tx.OpenBlocking`, `ClaimNotifySlot`, `PruneEvents` (bounded by id); `ClaimDeliveries` in a savepoint, rows sorted by `n`; `UpdateSession` delegates with no behaviour change; `TestSearchFindsDecidedOnly`, `TestSearchScopeTable`, `TestSearchRanksQuestionAboveContext`, `TestSearchStemming`, `TestSearchTagsAnyOf`, `TestSearchExcludeAndLimit`, `TestSearchHostileQuery`, `TestSearchSeesAnswerText`, `TestClaimTypingRequiresIdleOrNeedsDecision`, `TestRequeueOnlyUndoesTyping`, `TestCheckDecisionClaimsForOwnerOnly`, `TestTxClaimDeliveriesReturnsAndMarks` (with the mixed-status case), `TestTxOpenBlockingCounts`, `TestDeliveryClaimedExactlyOnce`, `TestUpdateSessionTxRollsBackOnError`, `TestUpdateSessionStillLosesNoUpdates`, `TestClaimNotifySlotOnePerWindow`, `TestPruneEventsBatches` pass under `-race`. | merged (PR #28), Opus, 1 fix rounds |
| 15 | `curia decisions` | Sonnet (expected, confirmed at dispatch) | `curia decisions ls [--status] [--json]` and `curia decisions edit` in `cmd/curia/decisions.go`, edit refusing through `refuseInsideAgent` (defined here, reused by T14 and T16); hand-written `cmd/curia/testdata/decisions.golden.json`; `TestDecisionsLsJSONGolden`, `TestDecisionsLsStatusFilter`, `TestDecisionsLsEmptyIsEmptyArray`, `TestDecisionsLsSweepsExpiry`, `TestDecisionsLsTableIsSanitised`, `TestEditRescope`, `TestEditSupersedeSetsSupersededAt`, `TestEditErrors`, `TestEditRefusesInsideAgent` pass. | merged (PR #27), Sonnet, 1 fix rounds |

### Wave 5

| Task | Title | Model (expected, confirmed at dispatch) | Done criteria | Status |
| --- | --- | --- | --- | --- |
| 12 | MCP tools and `curia mcp` | Opus (expected, confirmed at dispatch) | `mcp.Tools` (exactly four, exact descriptions, schemas and instruction strings), `mcp.Instructions`, caller resolved on every call in the order `CURIA_SESSION_ID`, the live session in `TMUX_PANE`, a live `CLAUDE_CODE_SESSION_ID`, unknown (plan decision 36), `curia mcp` pruning events at start; `TestToolSurfaceIsExactlyFour`, `TestRequestCreatesTicket`, `TestRequestInvalidListsAllProblems`, `TestRequestIdenticalRetry`, `TestRequestReturnsStrongPrecedent`, `TestRequestNoPrecedentAcrossRepos`, `TestRequestGlobalPrecedentAcrossRepos`, `TestRequestWeakMatchNotReturned`, `TestDiffersFromSuppressesThatPrecedent`, `TestTicketWithoutSessionIsAccepted`, `TestBlockingNotifiesOnce`, `TestCheckReturnsAnswerAndClaims`, `TestCheckByOtherSessionDoesNotClaim`, `TestCheckUnknownAndMalformedID`, `TestWithdrawByOwnerCitingPrecedent`, `TestWithdrawErrors`, `TestSearchUsesCallerRepoByDefault`, `TestToolsSweepExpiry`, `TestStoreErrorIsInternal`, `TestMCPOverPipes`, `TestCallerFromCuriaSessionID`, `TestCallerFromTmuxPane`, `TestCallerFromClaudeCodeSessionID`, `TestCallerUnknown`, `TestCallerAfterClear`, `TestCallerResolvedOnEveryCall`, `TestProjectDirDoesNotIdentifyACaller`, `TestRepoFallsBackToGit`, `TestNotifyRateLimited`, `TestMCPStartupPrunesEvents` pass. | merged (PR #30), Opus, 1 fix rounds |
| 13 | Hook wiring | Opus (expected, confirmed at dispatch) | `hook.Result`, `hook.Run` returning it; `Stop` through `UpdateSessionTx` with `ClaimDeliveries` and `OpenBlocking`; `state.Apply` honours `StopBlocked`; `cmd/curia/hook.go` prints the one JSON line first, before any tmux call, under the watchdog mutex (decision 0012); `TestStopBlockedIsWorking`, `TestStopBlockedBeatsOpenDecision`, `TestStopWithOpenBlockingNeedsDecision`, `TestStopWithOnlyAssumeIsIdle`, `TestStopWithAnotherSessionsTicketIsIdle`, `TestStopClaimsQueuedAndBlocks`, `TestSecondStopDoesNotBlock`, `TestStopHookActiveIsNotSpecial`, `TestStopDeliversSeveralInOneBlock`, `TestTwoStopHooksOneBlocks`, `TestOtherEventsNeverPrintOrClaim`, `TestStopOutputSurvivesHostileText`, `TestStopOnEndedSessionResumesAndDelivers`, `TestStopLatencyWithDecisions` (p95 < 20 ms, Plan 1's method, no retry; p50/p95 with and without `-race` reported), `TestStdoutOnlyOnBlockedStop`, `TestHookExitsZeroAndSilentWhenDecisionQueryFails`, `TestNothingPrintedAfterWatchdog` pass, with Plan 1's `TestHookLatency`. | merged (PR #31), Opus, 1 fix rounds |
| 14 | Delivery and `curia decide` | Opus (expected, confirmed at dispatch) | `deliver.Answer` in the plan's seven steps, the session's state re-read inside the claim immediately before typing (Enter approves a permission prompt); `curia decide` popup and flag forms, refusing through `refuseInsideAgent` before opening anything; marker failures logged at WARN; `newDecideSource`; `TestIdleSessionGetsTypedLine`, `TestNeverTypesIntoWaitingOrWorking`, `TestStateChangeBeforeClaimIsNotTyped`, `TestPaneGoneLeavesQueued`, `TestRecycledPaneIsNotTyped`, `TestSessionOutsideTmuxLeavesQueued`, `TestTmuxUnreachableLeavesQueued`, `TestSendKeysFailureRequeues`, `TestConfirmDeliversNothing`, `TestOverrideTypesRevertInstruction`, `TestMarkerReappliedAfterAnswer`, `TestAnswerErrorDeliversNothing`, `TestTypedLineReachesRealPane`, `TestHostileTextIsInertInARealShell`, `TestDecideRefusesInsideAgent`, `TestDecideOptionAnswers`, `TestDecideAcceptConfirmsAssume`, `TestDecideTextAndReasonFromStdin`, `TestDecideJSON`, `TestDecideFlagErrors`, `TestDecideAlreadyAnswered`, `TestDecideUnknownKeyNamesValidOnes`, `TestDecideOutputIsSanitised`, `TestDecideViaSSH`, `TestDecideSourceExpiresAndSorts` pass, the real-tmux ones not skipped where tmux is installed. | merged (PR #29), Opus, 2 fix rounds |

### Wave 6

| Task | Title | Model (expected, confirmed at dispatch) | Done criteria | Status |
| --- | --- | --- | --- | --- |
| 16 | Views: badge, dashboard column and the `d` key | Opus (expected, confirmed at dispatch) | `fleet.Count(sessions, openBlocking)`; `curia status` reads `CountOpenBlocking` and does not sweep; `ui.Source.OpenDecisions`; the `DEC` column (popup only; the sidebar does not call `OpenDecisions`) and `WithDecide`/`d`, not offered when `refuseInsideAgent` fails; `TestCountUsesOpenBlocking`, `TestBadgeCountsTicketsNotSessions`, `TestBadgeIgnoresAssumeAndPastDeadline`, `TestStatusStillSilentOnError`, `TestDecColumnShowsOpenCount`, `TestSidebarHasNoDecColumn`, `TestDKeyOpensDecideForRow`, `TestDecideClosedReturnsToDashboard`, `TestDKeyWithoutOpenDecisions`, `TestDKeyDisabledInSidebar`, `TestNarrowWidthStillFitsWithDec`, `TestOpenDecisionsErrorKeepsLastCounts`, `TestStoreSourceSweepsExpiry`, `TestNextJumpsToSessionWithBlockingTicket` pass; Plan 1's `TestBadge`, `fleet.Count` tests and `TestViewShowsRankedRows` (9 columns) updated. | not started |

### Wave 7

| Task | Title | Model (expected, confirmed at dispatch) | Done criteria | Status |
| --- | --- | --- | --- | --- |
| 17 | End-to-end acceptance | Opus (expected, confirmed at dispatch) | Hand-written fixtures in `testdata/decisions/` and golden files in `e2e/testdata/decisions/` (never generated from program output); `TestDecisionsEndToEnd` (script steps 1 to 11, private tmux server, MCP clients over real pipes) and `TestStopHookLatencyWithTickets` (at least 400 sequential processes, p95 < 50 ms) pass; `make check` passes. **Plan-level criteria (orchestrator):** `deadcode ./...` without `-test` on `plan-2` reports only deliberate test doubles (`tmux.Fake`); the isolated live check confirms the items plan section 14 marks for it. | not started |

## Plan 2 smoke checks

Run by the orchestrator on `plan-2` after each wave, with a temp `HOME`, `CURIA_DB` and a private tmux server (Plan 1's lesson: cross-task faults only show in a live binary).

| After wave | Check | Result |
| --- | --- | --- |
| 1 | `make check`; `curia install` into the temp home with a stub `claude` writes the skill and the `A` binding | |
| 3 | open the wave-3 database with the wave-1 binary's `curia ls` (old code, new schema) and the reverse: both work | |
| 4 | `curia decisions ls --json` on an empty and on a seeded database | |
| 5 | by hand over a pipe: `initialize`, `tools/list`, one `request_decision`; `curia decide D-1 --accept`; `curia hook Stop` prints one JSON line once | |
| 6 | `curia status` shows `D1`; the popup shows the `DEC` column and `d` opens the ticket | |
| 7 | Task 17, plus `deadcode ./...` without `-test`: only test doubles are reported | |

## Plan 2 Rulings

Format: Ruling: what — why — cost if wrong. Rulings 34 to 38 and 48 to 49 came from Plan 1's live check and are listed under Plan 1; Plan 2 numbering continues from 50.

39. Ruling: the pre-flight review's items 1-8, 11-13 and 15 are accepted as written (briefs include the cited section-4 parts; TestMain guard in cmd/curia; T15 forward refs cut; expire inside CreateDecision; bounded PruneEvents; copy-mode -q before typing; decide view pins selection; Stop emit first + savepoint; T5 failure rules; wave-5 name prefixes and one-at-a-time merges; small fixes; declared deviations incl. reasoning prompt on blocking answers) — each fixes a text defect that would otherwise cost a fix round — none beyond the plan text.
40. Ruling (changes review item 9, security-sensitive, owner away): `curia install` adds NO permission rules by default; `--allow-mcp-tools` adds the four exact tool names; without it install prints how to allow them — auto-allowing tools in the owner's settings is a security-sensitive default the owner has not approved — agents hit a permission prompt on first use of each tool until the owner opts in.
41. Ruling (rejects review item 14): T13's in-process latency gate stays one pass of 200 samples, p95 < 20 ms, no retry, as Plan 1's TestHookLatency — retry-until-pass weakens a gate; Plan 1's margin was large (p95 3-7 ms under race) — a loaded machine could flake it; then measure, do not loosen.
42. Ruling: the three desk-unsettled checks run before Wave 1 as live probes: MCP server environment, typing+Enter into a real prompt, `claude mcp get` output, plus Stop-hook block behaviour. Results amend T3, T5, T12, T13 before dispatch — each result changes task text, and building six waves on an unverified delivery design is the larger risk — one more step before Wave 1.
43. Ruling: MCP caller resolved on every tool call: CURIA_SESSION_ID, then the non-ended session bound to TMUX_PANE, then CLAUDE_CODE_SESSION_ID if a live row exists, then unknown — the env session id goes stale after /clear, the pane does not — two live sessions in one pane (nested) resolve to the most recently updated. (Plan decision 36.)
44. Ruling: MCP server answers any unknown request with -32601 at once and negotiates the protocol version — the real client opens with server/discover — none. (Plan decision 37.)
45. Ruling: SendKeys = three separate tmux invocations (copy-mode -q, send-keys -l, send-keys Enter), max 800 chars, refuses single characters; state re-read inside the claim transaction before typing — joined commands turn Enter into a newline; 1000+ chars become a paste; Enter approves a permission prompt — a session at a permission prompt whose state still says idle would get its prompt approved; the rule idle/needs_decision-only is the guard. (Plan decision 38.)
46. Ruling: install never runs `claude mcp get/list`; it reads mcpServers.curia from the Claude config JSON read-only and uses `claude mcp add/remove --scope user` — get/list start the server and hang 30 s on a silent binary — depends on the config file's location and shape. (Plan decision 40.)
47. Ruling: Stop hook ignores stop_hook_active; each Stop claims what is queued — two Stops per request are normal with subagents — none. (Plan decision 39.)

50. Ruling: `make check` runs all its gates under a lock shared by every worktree of the repository (`scripts/with-test-lock.sh`) — parallel implementers' checks were loading the machine during each other's latency tests and failing them; the gates and thresholds are unchanged — worktrees' checks run one at a time, which costs wall time.
51. Ruling: `sanitize.Keys` output is never typed on its own; only `decision.TypedMessage`'s sentence, which begins with the fixed words "Decision D-<n>", is typed, and `Keys` keeps Unicode letters as the plan says — the fixed lead means a typed line can never start with `/` or a command word, and ASCII-only would drop non-English answers — in a shell the line is a failed command named "Decision"; look-alike letters can appear in a typed answer.
52. Ruling: the sanitiser maps Unicode space separators to a plain space before filtering — the plan's rule glued words together across a non-breaking space — none.
53. Ruling: an ESC followed by a line break consumes only the ESC — the plan's two-byte rule swallowed line breaks in multi-line text — none.
54. Ruling: `tmux.ErrPartialSend` marks a `SendKeys` failure after which text may be sitting in the prompt; callers must not type again — a retry would append the answer to the leftover text and submit a doubled line — a partial send leaves the answer for the user to see and submit or clear.
55. Ruling: `Bell` writes only to `/dev/ttysN` or `/dev/pts/N`, checks the handle is a character device, and uses a write deadline — the plan's pattern also accepted `/dev/fd/1`, `/dev/null` and block devices — a platform with other pty names gets no bell.
56. Ruling (amends Plan 1's settings path): there is one Claude directory, `$CLAUDE_CONFIG_DIR` when set, otherwise `~/.claude`, holding `settings.json` and `skills/` for install and uninstall — otherwise a user with `CLAUDE_CONFIG_DIR` set gets the MCP server registered where Claude looks and everything else where it does not, with install reporting success — it assumes, without a probe, that Claude Code reads settings and skills from that directory.
57. Ruling: the "tools not allowed" hint is printed only when at least one of the four tool rules is missing — a plain install after `--allow-mcp-tools` said the opposite of the truth — none.
58. Ruling (extends 54): `ErrPartialSend` means "text may be in the prompt, unsubmitted or even submitted"; it is returned when Enter fails and when the typing step fails with its context already done — a killed `send-keys` client still types its text — a delivery interrupted by a timeout is never retried by typing; it stays reachable through `check_decision` and the Stop hook.

59. Ruling: in the message a stopping agent receives, each decision is one line with the option's label in square brackets after the key (`Decision D-117 answered: option B [Postgres]. Reasoning: ... Continue.`); free text has no bracket; multi-line text is flattened — the plan said "the full label in brackets" without a form, and labels can contain commas — these strings are compared byte for byte by later tests.
60. Ruling: the notification body omits the repo part when it is empty — tickets from an unidentified caller have no repo and would read "D-7 .: question" — none.
61. Ruling: a precedent token is a run of letters, marks or numbers — FTS5's tokenizer keeps marks and numbers inside tokens, so the plan's letters-and-digits rule split Indic words and decomposed accents and lost recall — none known.
62. Ruling: precedent tokens over 64 runes are dropped, and an empty match expression means "do not run the query" — `MATCH ''` is an FTS5 syntax error and a 1 MB word became a 1 MB term — a very long identifier cannot be searched for.
63. Ruling: after the user's own answer, answer keys are ignored during a 750 ms quiet period that each ignored key restarts — working down a queue should not cost a wasted keypress per ticket, and a double-tap or held key must not answer the next one — the user pauses 750 ms between answers.
64. Ruling (amends plan 4.5): a superseded decision is not a duplicate; an identical retry after supersession opens a new ticket — supersession is the plan's remedy for a wrongly keyed answer and its way to unblock a re-ask — an agent can re-raise a question the user retired.
65. Ruling: a non-empty `SupersededBy` implies supersede in the store; a repeated supersede keeps the first timestamp — the CLI flag implies it — none.
66. Ruling (the dwell rule): an answer key acts only when the displayed ticket has been unchanged for at least 250 ms, and `y` confirms only 250 ms after its prompt opened — Bubble Tea draws on a timer, so keys arriving in one read could answer a ticket or confirm a prompt that never reached the screen — the popup cannot be driven by fast scripted input; the CLI flags are the scripting path.
67. Ruling: every refused answer key restarts the dwell clock — a held, auto-repeating key otherwise outlasts each gate in turn — impatient repeated pressing is held off until a pause.
68. Ruling: an answer key refused because of a change the user did not cause starts the same 750 ms quiet period — terminals send no key-up, so a held key's first repeat after the operating system's 375 to 660 ms delay looks like a deliberate press — after any queue change that a key ran into, an answer needs a 750 ms pause; a keyboard with a repeat delay above 750 ms can still get one repeat through.

## Plan 2 Open Questions

Seeded from plan section 14. Each has a default, so execution is not blocked.

1. **Settled 2026-11-11 (live probe, plan decision 36):** the server inherits `claude`'s environment; `TMUX_PANE` is correct, `CLAUDE_CODE_SESSION_ID` goes stale after `/clear`, `CLAUDE_PROJECT_DIR` is the session cwd. As first asked: **Does Claude Code pass its environment to a stdio MCP server?** The docs state only that `CLAUDE_PROJECT_DIR` is set. If `TMUX_PANE` or `CLAUDE_CODE_SESSION_ID` is not inherited, the caller falls back to matching the project directory, which fails when two sessions share a directory. Default: the five-step order of Task 12. Live check: confirm which step resolves a real session; if none does reliably, Plan 3's `curia spawn` must set `CURIA_SESSION_ID` and manual sessions need another channel.
2. **How hard should the "no self-answering" boundary be?** An agent with a shell can run `curia decide` or write to the SQLite file. Default: the `CLAUDECODE` guard plus `answered_via = cli` on every flag-form answer (decision 0013). A real boundary needs something an agent cannot reach. Is the guard enough for V1?
3. **Spec section 7 against a pane that has become a shell.** Default: the restricted `sanitize.Keys` alphabet, at the cost of lossy typed text (decision 0013). The alternative is to type only `Decision D-117 answered. Call check_decision D-117.`
4. **Spec section 5 against the schema here** (extra columns, `n` as the primary key; decision 0014). Default: as written; should `docs/DESIGN.md` be updated to match?
5. **Confirmed by the live probe (plan decision 39):** a block with exit 0 continues the same turn. **Plan 1's hook rule against F7.** Default: the amended stdout rule (decision 0012).
6. **Badge meaning.** Spec F4 says `D2` is "decisions"; Plan 1 counted sessions in `needs_decision`. Default: open blocking tickets.
7. **F17 "soonest first" for cold caches.** Default: a cold cache sorts with unknown ones, by age.
8. **Repo identity across worktrees.** `sessions.repo` is the worktree's top-level path. Default: leave it; Plan 3 must store the main repository before it spawns worktrees.
9. **Auto-allowing the tools.** Orchestrator ruling with the owner away: off by default; `curia install --allow-mcp-tools` adds the four exact tool names, and uninstall removes those four strings (plan decisions 26 and 35). Should install allow them by default, and is the server-wide `mcp__curia` ever wanted?
10. **Settled:** the Stop payload carries `last_assistant_message`, and Plan 1's live-check fix reads it. **Stop reads the transcript for the last assistant text**, but Claude Code's docs say the payload's `last_assistant_message` should be used at Stop. Not a Plan 2 change; default: leave it and do a small fix PR.
11. **`curia install` path problems inherited from Plan 1** (Plan 1 Open Questions 8, 14, 16): the MCP registration and the `A` binding record the same resolved binary path as the hooks. Default: same behaviour as the hooks.
12. **The key for the decide popup.** Default: `prefix + A`.

Items for the isolated live check (could not be settled from the desk):

- **Settled (plan decision 38):** text and a separate Enter submit with no pause up to at least 800 characters; one tmux invocation does not.
- **Partly settled (plan decision 39):** the block works with a background subagent running, whose result comes as its own turn and `Stop`; a foreground subagent was not observed.
- **Settled (plan decision 40):** `claude mcp get` starts the server and prints paths with spaces ambiguously; install reads the config JSON instead.
- **Partly settled:** a short `instructions` string reached the model verbatim; the full one (about 800 characters) is checked in Task 17's live check.

Remaining risks after the live probes (plan section 14): the `permission_prompt` Notification arrives about 6 seconds after a prompt opens, so the store may still say `working` meanwhile (the typing rule covers it; no hook closes the window, Ruling 48); copy mode with a client attached is untested; the paste threshold lies between 800 and 1,000 characters; `claude mcp get`/`list` run by the user start `curia mcp`, which then prunes old events; Claude Code labels a Stop block's reason "Stop hook error" (cosmetic).

## Plan 2 Ledger

- Wave 3 merged 2026-11-12: Task 9 (PR #26), Task 10 (PR #25, four fix rounds, the fourth by a fresh Fable implementer). Smoke check passed: a database created by the new binary is read and written by the old one, and one created by the old binary is migrated and read by the new one, with no error logged.
- Wave 2 merged 2026-11-12: Task 6 (PR #24), Task 7 (PR #22), Task 8 (PR #23).
- Wave 4 dispatched: Task 11 (Opus), Task 15 (Sonnet).

- Wave 1 merged 2026-11-12: Task 1 (PR #19), Task 2 (PR #17), Task 3 (PR #20), Task 4 (PR #18), Task 5 (PR #21). Smoke check passed: `make check` on `plan-2`; `curia install` and `uninstall` into a temp home with a stub `claude` wrote and removed the skill, the `A` binding and the MCP registration call.
- Wave 2 dispatched: Task 6 (Opus, raised from Sonnet by the pre-flight review), Task 7 (Sonnet), Task 8 (Sonnet).

One line per task: wave, worktree, branch, status, model, fix rounds.

(none yet)

## Plan 2 Next steps

1. Merge the setup PR (`chore/plan2-setup`) into `main`, then branch `plan-2` from `main`.
2. Dispatch Wave 1 (Tasks 1 to 5), confirming each model first; smoke check after the wave.
3. Owner: run `curia install` on the real machine and the three-session check; add Go 1.26 to PATH; choose a remedy for Plan 1 Open Question 19.

# Plan 1 (Foundation): complete

Plan 1's record, kept as it stood when Plan 1 closed, plus its live check (Rulings 34 to 38, 48, 49; Open Questions 19 to 21). Its waves, done criteria and models are in the Plan 1 plan and in the results table below.

## Plan 1 status

- **Plan 1 (Foundation): complete.** All eleven tasks, the final whole-branch review and its fix wave are merged into `plan-1` (PRs #2 to #13). The fix-wave re-review found every finding addressed and nothing new at Critical or Important level.
- `deadcode ./...` without `-test` reports only `tmux.Fake`, the deliberate test double.
- The owner (2026-11-11) authorised the orchestrator to merge its own PRs, including `plan-1` into `main`, and to write and execute Plan 2 and later plans. An isolated live check (private tmux server, temp HOME) is authorised; the real `curia install` stays with the owner.
- Merged to `main` (PR #14).
- Isolated live check done (method of Ruling 33; real Claude Code sessions on a private tmux server). Its fixes are merged to `main` (PR #15; Rulings 34 to 38, 48, 49).
- The real `curia install` check on the owner's machine is still the owner's.

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

## Plan 1 Open Questions

1. Resolved by Ruling 4.
2. Go 1.26 is keg-only via Homebrew on the dev Mac and not on PATH by default; owner to add it to their shell config (dotfiles are chezmoi-managed).
3. `TestHookLatency` (Task 5, p95 < 20 ms in-process) runs under `-race` in `make check` and may be flaky, especially on CI runners. Decide at Task 5 dispatch how the budget is measured, without skipping or loosening the test.
4. Resolved by Ruling 5.
5. `go.mod` says `go 1.26.0` plus `toolchain go1.26.11` rather than bare `go 1.26` (decision 0001).
6. Resolved by Ruling 23.
8. `curia uninstall` only removes entries for the current binary path (Ruling 13); entries from a moved or rebuilt binary stay behind and fire against a missing file. Decide whether uninstall should also match older paths.
9. Under sustained heavy write load a waiting SQLite writer can time out (SQLite polls for the lock rather than queueing). Hooks are short so this is unlikely in practice; revisit if the log shows busy errors.
10. Resolved by Ruling 28 for a restarted server. `fleet.Reconcile` matches sessions to panes by pane id alone. A restarted tmux server numbers panes from `%0` again, so an old session whose id is reused is never marked stale and `next` could jump to an unrelated pane. The data model has no server identity; decide in the final review or Plan 2.
11. The `events` table keeps full hook payloads (up to 1 MiB each, including tool output) with no pruning. Add retention in Plan 2. **Addressed by Plan 2:** Task 11 adds `store.PruneEvents` and Task 12 runs it (7 days, in batches) when `curia mcp` starts (plan decision 30).
12. A session outside tmux that dies without SessionEnd keeps its last state forever (it is never stale by design), so a waiting one pins `W1`. An age-out or a `curia forget` belongs in a later plan.
13. Two tmux servers on different sockets can both have a `%1`; telling them apart needs a `tmux_server` column (restore plan).
14. `curia install` records the resolved binary path; an upgrade that changes the path orphans the old hook entries (see Ruling 13). Decide at packaging time whether to match any `curia hook <Event>` entry.
15. SQLite runs with the default `synchronous=FULL`; unmeasured on Linux. Consider `NORMAL` if hook latency is a problem there.
16. A `#` or `%` in the binary path is not escaped for tmux formats in the snippet (hooks in `settings.json` are unaffected). `curia install` should refuse or warn on such a path.
17. A session waiting on a permission prompt is ended and hidden when a background `claude -p` starts in its pane, until its next event (Ruling 27 restores it only then).
18. `SubagentStop` on an ended session is ignored, so `subagents_active` can read high after a resume.
19. A denied permission prompt leaves the session `waiting` (holding `W` and attracting `next`), and an Esc interrupt leaves it `working`, until the next prompt in that session; no Claude Code hook fires for either. The transcript shows a `user-rejected` tool result and "[Request interrupted by user...]" lines about 200 ms after the keypress, so a correction from the transcript when a view refreshes is possible, as is pane scraping. Owner to choose (Ruling 49).
20. `docs/DESIGN.md`'s F1 table still says SessionStart means `working`; the code follows Ruling 37 (`idle` and seen). The spec is the owner's to update.
21. A SessionStart with `source: compact` in the middle of a turn shows the session idle until its next tool event.
7. No LICENSE file: the owner chose to leave it undecided (2026-11-11).

## Plan 1 Rulings

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
33. Ruling: the isolated live check runs real Claude Code with the real HOME for its own login but loads hooks only through `claude --settings <temp file>` whose hook commands set HOME and CURIA_DB to a temp dir, on a private tmux server — a temp HOME has no credentials — the test sessions' transcripts land in the real `~/.claude/projects`.
34. Ruling (narrowed by Ruling 48): register whichever extra Claude Code hook events fire on a denied permission or an Esc interrupt; such an event sets the session idle and seen; a plain tool failure keeps working; an event at the moment a permission prompt opens sets waiting at once; if no event exists, leave unfixed and report (no pane scraping, polling or transcript watcher) — live check: denial left `waiting` forever, Esc left `working` forever — more hook entries in settings.json; depends on events Claude Code may change.
35. Ruling: the snippet raises status-right-length to 80 when below 80 — default length 40 hides the badge — changes a tmux option the user did not ask about.
36. Ruling: Stop uses the payload's last_assistant_message (first line) for last_update, transcript as fallback — the transcript is not flushed when Stop runs — none.
37. Ruling (amends spec F1 table): SessionStart sets idle and seen, not working; first prompt makes it working — a fresh or /clear session sits at an empty prompt — goldens and the e2e script expectations change.
38. Ruling: tmux.Client gains PaneFocused; a marker about to turn red on the focused pane is marked seen instead — red dot on the window being watched never cleared — one tmux call on marker-changing events.
48. Ruling (narrows Ruling 34): PermissionRequest is NOT registered — approving fires no hook, so an approved long tool would show `waiting` for its whole run; a false alarm for minutes is worse than a 6 s delay — a real prompt is shown up to 6 s late (the Notification).
49. Ruling: denied-prompt-stuck-waiting and Esc-stuck-working are left unfixed in Plan 1 and recorded as Open Questions for the owner with the options (transcript-based correction during reconcile/status; pane scraping; accept) — no hook exists and both remedies change the architecture ("nothing scrapes panes") — a denied prompt shows W and attracts `next` until the user's next prompt in that session; an interrupted session shows working until then. (Open Question 19.)

## Plan 1 deferred minors

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
- Live check, not fixed (defect 7): a waiting session's last_update becomes the generic "Claude needs your permission", overwriting the tool and target.
- Live check, not fixed (defect 10): internal tool names leak into last_update (`SubagentHandback` after a subagent), and subagent tool events overwrite the main session's last_update.
- Live check, not fixed (defect 12): a stale row's AGE resets to 0s when it is marked stale, so it looks freshly active.
- Live check, not fixed (defect 13): at about 50 columns every dashboard column truncates and rows cannot be told apart; a narrow layout should drop columns.
- Live check, not fixed (defect 14): SessionStart p50 27.8 ms, p95 93.6 ms at load average 11.9, over the 20 ms budget; re-measure on an idle machine before acting.
- Live check, not fixed (defect 15, design limit): a Claude process killed with SIGKILL in a pane that stays open stays `working` forever; a liveness check of the pane's foreground process would catch it.
- Live-check fixes review: SessionStart with source=compact mid-turn shows idle-seen until the next tool event (Parse should read source; keep state on compact; Open Question 21); MarkSeen on focus uses time.Now() and can cover a concurrent event (use sess.UpdatedAt); PaneFocused means a client is attached, not that the terminal has focus; the focus path adds a second tmux call on red-turning events, unmeasured; the MarkSeen-failure branch is untested; docs/DESIGN.md still says SessionStart -> working (Open Question 20).
## Plan 1 ledger

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
