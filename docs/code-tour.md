# Code tour

Three features followed through the code, in order. Names were checked against the files on `main`; open the function, then read its callers. Tests are named for behaviour, so `go test -list '.*' ./internal/mcp` is a second map.

## A. A decision ticket: agent asks, user answers, agent hears

1. `internal/mcp/tools.go` `Tools`: the seven tools; `tools.wrap` adds the expiry sweep to each handler.
2. `tools.request` (same file): the `request_decision` tool validates through `internal/decision`, stores, notifies, then looks for precedent (`strongPrecedent`).
3. `internal/store/decisions.go` `CreateDecision`: the one write that makes the ticket; a duplicate returns the open one.
4. `cmd/curia/mcp.go` `mcpNotify`: wired as `Notify`; queues the phone push, then rings the pane bell and notifies macOS if this process wins the shared rate-limit slot.
5. `cmd/curia/decide.go` `runDecide`: `curia decide` (popup, or flags through `decideAnswerFromFlags`); refuses inside an agent session.
6. `runDecideAnswer`, then `decideSource.Answer`: hands the answer to `internal/deliver`.
7. `internal/deliver/deliver.go` `Answer`: `Store.AnswerDecision` (`internal/store/decisions.go`) records it, then `delivery.answer` tries to deliver.
8. Typed path: `Store.ClaimTyping` (`internal/store/delivery.go`) claims it once, `delivery.typeLine` types it only into an idle pane.
9. Queued path: the agent's next Stop calls `cmd/curia/hook.go` `runHook`, then `hook.Run` and `runStop` (`internal/hook/run.go`).
10. `claimStopDeliveries` calls `Tx.ClaimDeliveries` (`internal/store/tx.go`); `blockLine` prints the one block line that carries the answer.
11. Or the agent asks: `tools.check` (`internal/mcp/tools.go`, the `check_decision` tool) returns the answer, also when it was already delivered.

Where I would get lost: the hook's Stop path. Read `runStop` first, and know that `apply` receives a callback which claims deliveries inside the session's transaction and returns whether a blocking ticket is open; the output line is written only after the transaction commits. Then read `claimStopDeliveries`' doc comment.

## B. A review: agent says done, user approves, the merge, the board

1. `internal/mcp/complete.go` `complete` (the `complete_task` tool): says "ready for review"; it runs no git.
2. `internal/store/reviews.go` `CompleteTask`: opens or updates the one pending review of the task; `Reviewed` (`mcpReviewed`, `cmd/curia/mcp.go`) refreshes the marker and push.
3. `cmd/curia/review.go` `runReview`: `curia review`, user-only; `reviewRunView` opens the view (`ui.NewReview`), `reviewRunAction` is the flag form.
4. `reviewSource` (same file): the glue between the view and the store and git; its `Approve` calls `reviewApprove` with the head the view showed.
5. `reviewApprove`: `reviewFoundDone` (already merged?), `reviewCheck` (`cmd/curia/review_git.go`: branch, idle agents, clean worktree), `reviewApproveReady`.
6. `Store.ClaimReviewIntent` (`internal/store/reviews.go`): one conditional UPDATE records the intent to merge that exact commit.
7. `reviewApproveMerge`: runs `gitops.Merge` (`internal/gitops/merge.go`), `git merge --no-ff` of the approved commit only; on failure `reviewMergeRelease` undoes or keeps the claim.
8. `Store.CloseReview` marks the review merged.
9. `internal/task/status.go` `Derive`: a newest review that is approved or merged makes the task `done`; nothing stores "done", so the task leaves the board's open columns by itself.

Where I would get lost: the approve path. Start at `reviewApprove` and read only its calls in order; `reviewSource.Approve`, the view's confirm (`ReviewModel.confirm`, `internal/ui/review.go`) and the `after` closure sit around it. Review code lives in `cmd/curia/review*.go` and may be split or moved to its own package; search for `reviewApprove` rather than trusting line numbers.

## C. The dashboard: popup key, inbox, a view, a key that changes something

1. `internal/install/tmuxconf.go` `keyStates`: the `display-popup` binding that runs `curia ui`.
2. `cmd/curia/ui.go` `runDashboard`: builds `ui.New` (`internal/ui/model.go`) and adds one `WithX` factory per view (`WithInbox`, `WithDecide`, `WithReview`, `WithBoard`, and so on); a factory is left out when the dashboard is read-only.
3. `Model.Init`: with an inbox factory the first screen is the inbox (`openInboxMsg`, then `showChild`); `Tab` shows the session list.
4. `InboxModel.key`, then `act` (`internal/ui/inbox.go`): an item opens through `InboxOpenMsg`, and `Model.openFromInbox` calls `showChild` with the decide or review view.
5. Or from the list: `Model.Update`, `keyMsg`, `updateKey`, `commandKey`; `lookupCommand` (`internal/ui/commands.go`) finds the view registered for the key (`d` is registered in `decide.go`'s `init`) and runs `Model.openDecide`.
6. `DecideModel.updateKey` and `commandKey` (`internal/ui/decide.go`): a number key goes to `answerOption`, then `startAnswer`.
7. `guard` refuses an answer the view may not give; `proceed` opens a `confirmGate` for an irreversible one (`internal/ui/confirm.go`: dwell, quiet period, burst gap).
8. `confirmKey` accepts `y` only through the gate; `send` calls `DecideSource.Answer`, which is `runDecideAnswer` in `cmd/curia/decide.go`, so feature A continues at step 6.

Where I would get lost: the dashboard model's routing. `Model` is a list view plus a router: a child view (inbox, decide, review, board) takes over `Update` and `View` until it closes (`showChild`, `updateChild`, `wrapChild`). Read `showChild` and `updateChild` before any key handler, and read `confirm.go`'s doc comment before touching a prompt.
