# 0012. The Stop hook writes a decision

Status: accepted (Plan 2 setup)

## Context

Plan 1's hook never wrote to stdout. Spec F7 delivers an answer to an agent that has finished its turn, and Claude Code's Stop hook can block a stop and hand Claude a reason, which needs one line on stdout. An answer can also be typed into an idle pane, or fetched by the agent itself. Whichever path delivers it, the agent must not get the same answer twice.

## Decision

- **Amended stdout rule** (plan section 2, word for word): `curia hook <Event>` writes nothing to stdout or stderr and exits 0 on every path, with one exception: `curia hook Stop` writes exactly one line to stdout, `{"decision":"block","reason":"<text>"}` followed by a newline, when and only when its store transaction claimed at least one queued answer for that session. The exit code is still 0. Nothing is ever written to stderr.
- An answer that needs delivering is stored with `delivery = queued`. Three paths can claim it, each in one write transaction that succeeds only while `delivery = 'queued'`:

  | Path | Who | When |
  | --- | --- | --- |
  | `send_keys` | `curia decide`, right after answering | the session has a live pane and is `idle` or `needs_decision` |
  | `stop_hook` | `curia hook Stop` | the session stops with queued answers; all of them go into one reason |
  | `check` | `check_decision` from the asking session | the ticket is decided and still queued |

- Delivery is at most once: the claim is made before any output. A process that dies between its claim and its output loses that delivery; `check_decision` still returns the answer, and the agent instructions tell agents to call it when a blocking answer has not arrived. If `SendKeys` fails after its claim, the claim is undone (`RequeueDelivery`).
- The Stop hook adds two statements to the transaction it already holds and nothing to any other event. The budget is unchanged: p95 under 20 ms in-process, under 50 ms per process.

## Consequences

- A duplicate blocked stop or a line typed twice cannot happen; a rare lost delivery can, and the agent can recover it.
- `cmd/curia/hook.go` prints the line immediately after `hook.Run` returns, before any tmux call, under the same mutex as its watchdog; if the watchdog has already ended the run, nothing is printed and that delivery is lost (`check_decision` still returns the answer).
- The Stop hook's claim runs inside a savepoint: if it fails, nothing is claimed and the session update still commits.
- Claude Code ends the turn after 8 consecutive blocked stops; one Stop blocks at most once however many answers were waiting.

Amended 2026-11-19 (owner, ticket D-1, answer "A, but pass on the median of three batches, not any one"): the latency budget method. A latency gate runs three batches, each with the gate's own sample count, and asserts that the median of the three batch p95 values is under the same threshold (20 ms in-process, 50 ms per process). Always three, never an early exit; the batches' p50, p95 and max are logged. Thresholds, samples per batch, warm-up and the race detector are unchanged, and a regression slows every batch, so it still fails the median. Why: on GitHub's shared runners one batch's p95 is sometimes 4 to 20 times its median from runner noise, with no regression. The helper is `internal/latency` (e2e keeps a copy, as it may import no internal package). If flakes continue the owner's next step is to gate on the ratio to a reference workload measured in the same job; it is not built.

Amended 2026-11-19 (owner decision D-1, its fallback, now built): the median did not cure it, because GitHub runners differ in raw speed from run to run (one run measured the same ingest gate with p50 14.65 ms, another 19.36 ms, all three batches agreeing). A gate now asserts that the median of the three batch p95 values is under threshold x scale, where scale is the p95 of a reference workload measured in the same process (individual durable write transactions on a fresh WAL store through the pure-Go driver, private schema, not `internal/store`; before and after the batches, the smaller taken) over its nominal 2 ms, clamped to [1, 2]. The tail, not the median, because the gates fail on an occasional slow commit. Never below 1, so a fast machine keeps the absolute threshold; a reference tail over twice nominal fails the gate. The reference, scale and effective threshold are always logged. Thresholds, samples, warm-up and the race detector are unchanged; a product slowdown raises the measured value and not the reference, so it still fails.
