# 0015. Plan 3 package boundaries

Status: accepted (Plan 3 setup). Extends decisions 0003 and 0010, whose rules and details still hold.

## Context

Plan 3 adds four packages of pure rules: `task` (ids, validation, slug, branch, worktree path, tmux names, status derivation, the brief, the Backlog.md parser), `message` (text, address grammar, typed line, Stop block), `route` (candidates, name resolution, settling a pick, the model prompt and its parser, the accuracy gate, the auto-send rule) and `team` (parsing a team config, assigning members to sessions). As in Plans 1 and 2, parallel implementers do not see each other's code, so the order of the plan's File structure table has to be enforced by the gate. The store applies task, message and team rules inside its own write transactions (id allocation, the launch claim, message claims, team assignment), so these packages must sit above `store` and do no I/O of their own.

## Decision

Table order (21 packages, then the command): `model`, `config`, `sanitize`, `decision`, `precedent`, `task`, `message`, `route`, `team`, `store`, `state`, `transcript`, `hook`, `tmux`, `fleet`, `attention`, `notify`, `deliver`, `mcp`, `ui`, `install`, `cmd/curia`.

| Package | Third-party allowed | Internal packages allowed |
| --- | --- | --- |
| internal/task **new** | none | model to precedent |
| internal/message **new** | none | model to precedent, task |
| internal/route **new** | none | model to precedent, task, message |
| internal/team **new** | none | model to precedent, task, message, route |
| internal/store and every package below it, and cmd/curia | as before | as before, plus task, message, route, team |

All other rows of decision 0010's table are unchanged.

- Every new rule is `list-mode: strict` with exact-match internal entries, and each `deny` names this record. Every existing rule from `store` down gains four allow lines; `model`, `config`, `sanitize`, `decision`, `precedent` and `e2e` are unchanged.
- The `unlisted` catch-all excludes the four new directories.
- `scripts/check-layout.sh` lists the same 21 packages (decision 0006).
- The depguard rules and the layout allow-list name directories that do not exist yet. That is safe: depguard matches the files it is given, and the layout check only rejects paths that exist and are not listed. Decision 0010 did the same for its six packages before their tasks landed.

## Consequences

- Verified during setup with temporary probe files and `make check` (all removed afterwards): `internal/task` importing `internal/store` fails; `internal/route` importing `internal/tmux` fails; `internal/team` importing cobra fails; `internal/store` importing `internal/message` and `internal/team` passes; `internal/ui` importing `internal/route` passes.
- Four more rows of rules for four small pure packages. A later plan that adds a package or a module still needs a rule change by the orchestrator.
