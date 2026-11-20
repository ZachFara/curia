# 0029. One-way exports

Status: accepted (Plan 5 setup). The plan numbers this record 0027. Probe O1 (the owner's) and A1 (optional) have not run.

## Context

Spec F13: decisions export to Obsidian, and a global decision can become an Agent Commons entry. Ticket text is untrusted (spec section 7), and a vault with Dataview or Templater interprets a great deal. Agent Commons's `propose` takes `symptom`, `environment`, `cause`, `fix` and `domains` plus the proposing agent's own `task_id` and `model` (verified at the desk).

## Decision

- **Obsidian.** `[obsidian] dir` names an existing folder (empty: off; Curia never creates it). Every decided decision (answered, confirmed, overridden; standing rules included) is written as `<id> <title>.md`, again after a rescope or supersession; withdrawn and expired tickets never. Every agent- or user-written string is inside a plain code fence. Notes are written through `os.Root`; an existing note is rewritten only when it holds Curia's marker (`curia_id: <id>` in its first 20 lines); a note without it is left alone and reported as a foreign note; nothing is deleted. The sweep runs after `curia decide` and on `curia export obsidian`; no other process writes files.
- **Agent Commons.** `curia export commons <id>` prints the arguments of `propose` for a decided, non-superseded, global decision and sends nothing. No repository path, task id, session id or branch is included, and every string is cleaned. Publishing is one more step the owner takes (hand the JSON to an agent that has the commons tools).
- Nothing is read back from either destination.

## Consequences

- Notes are not pretty markdown and links in reasoning are not clickable.
- A hand-made note under a decision's id blocks that decision's export until renamed.
- A decision made from the phone reaches the vault at the next answer at the desk or the next sweep.
- A direct publish needs a network client and a stored credential and is one task after V1 if the owner asks (plan question 4).

Amended 2026-11-19 (Plan 5 records; Rulings in `PROGRESS.md`):

- Ruling 175 (PR #139): the export mark is guarded (`MarkExportedIf`, one transaction), so a rescope between the write and the mark leaves the decision due. A decision whose note name is taken by a foreign note is skipped and remembered (a `meta` row `export_skip:<id>`, no migration), so "blocks that decision's export until renamed" above no longer means it is retried at every sweep: `curia export obsidian --retry-skipped` tries the skipped ones again. Answering in the dashboard's decide view starts a background sweep. The skip is printed by `curia export obsidian`; printing it when answering at a terminal is a fix wave item.
- Probes O1 (the owner's) and A1 (optional) have not run.
