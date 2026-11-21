# 0038. The monitor: one process, jobs that claim their work

Status: accepted (2026-11-20, Rulings 205, 210 and 212; code in `cmd/curia/monitor.go` and the jobs' files).

## Context

Resource sampling, schedules, container sampling, the end of an away period and auto-spawn all need a loop that runs while the user is away from the keyboard. Nothing else in Curia may depend on it.

## Decision

- `curia monitor` is one process holding an exclusive `flock` on `monitor.lock` in the database's directory (not the state directory). `monitor start` and `status` are not user-only (tmux `run-shell` has no terminal), `stop` is, and it checks the pid belongs to a Curia monitor before signalling. `monitor.autostart` (default true) lets a tmux `client-attached` hook (index 78) start it; the check is made at run time. `curia uninstall` stops it.
- Every `resources.interval` (10 s) it runs `monitorJobs` in order, each under its own 20 s deadline; a failing job is logged and the loop goes on. A new job is one entry in that slice. Current jobs: sample resources, prune resources (24 h or 100000 rows), fire schedules, sample containers, end away, auto-spawn (its own 30 s tick).
- The lock makes a second monitor impossible, but work is still claimed with one UPDATE so a restart, a race or a second claimer cannot repeat it: `ClaimSchedule` (moves `next_run_at` and records the run in one write), `AutoClaim` (see 0035), the away end (`EndAway`). The claim comes before the side effect.
- A sample is shown only when fresh (3 x the interval); otherwise the dashboard shows `?`.

## Consequences

- Pid reuse of a pane process is not detected (marked `shortcut:` in the code). `ps` `%cpu` is ps's own average.
- Without a monitor nothing fires: no schedule, no automatic end of away (the hook still stops applying away at the deadline), no auto-spawn.
