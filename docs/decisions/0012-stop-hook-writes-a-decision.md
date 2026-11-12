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
- `cmd/curia/hook.go` prints the line under the same mutex as its watchdog; a claim made after the watchdog fired is logged as `delivery claimed but not printed`.
- Claude Code ends the turn after 8 consecutive blocked stops; one Stop blocks at most once however many answers were waiting.
