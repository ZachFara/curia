# 0039. Settings layers: a repository can only tighten

Status: accepted (2026-11-20, Ruling 194; code in `internal/config/layers.go`, `policy.go`, `cmd/curia/config.go`, `settings.go`, `internal/store/config_revisions.go`).

## Context

A repository can hold a `.curia/config.toml`, and anything in a repository can be written by an agent. A setting that widens what an agent may do must never come from there.

## Decision

- `config.Resolve` layers defaults, the user file, the repository file, then flags, and reports each key's source, refusals and problems. The repository file is read with a 64 KiB cap and strict TOML; symlinks (file or parent) are refused; each key is applied on a trial copy and the result re-validated. Unknown keys are reported and ignored.
- `keyPolicy` in `policy.go` has one line per key, a compiled-in table: `PolicyNone` (the default: a repository cannot change it), lower, raise, false-only, true-only, a level that may only drop, and rules that apply to that repository only. `TestEveryKeyHasAPolicy` fails when a key has no line. A new key is a struct field, a default and one policy line; it then shows in `curia config list` and the settings view by itself.
- Agents never change settings: `config set`, `edit`, `rollback`, `auto-spawn on|off` are user-only, the hook denies agent edits of Curia's directories (0033), and the dashboard's settings editor goes through the same function (`configSetKey`) under the shared confirm gate (0040). Never editable from the view: `editor`, `review.diff_tool`, `costlens.analyzer`, `db_path`, `log_path`, `serve.listen`, `routing.auto_send`, lists and tables.
- Every change to the user file is a revision (`config history`, `rollback`); before the first write, or after a hand edit, the file as it stands is stored as a `baseline` revision, so `rollback` restores the original byte for byte; an edit made outside Curia is noticed (`C!` in the status badge, `config doctor` acknowledges it).

## Consequences

- `config set` re-encodes the file and drops its comments and says so (the baseline keeps them, so rollback restores them). Revisions beyond the newest 200 are pruned; baselines never. Tables and lists are set only with `config edit`.
- The spec's "nothing" rows are `PolicyNone`; the few tightenings are listed in `policy.go`, not here.
