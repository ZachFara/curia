# 0008. Plan 1 builds bottom-up; outside-in within each task

Status: accepted (Phase 1 setup)

## Context

Plan 1 runs in five waves with up to five implementers in parallel. Outside-in across the whole plan (start from `curia hook` and work down) would serialize everything behind the hook command. The plan's File structure table already orders packages so lower ones depend only on those above.

## Decision

- Plan 1 builds bottom-up across tasks: shared types and config, then store, state machine, transcript parser, tmux adapter and install (Wave 2), then the hook command and fleet (Wave 3), then attention and the UI (Wave 4), then the end-to-end replay (Wave 5). This is what lets a wave run in parallel.
- Within each task, build outside-in from the task's entry point, or from the exported functions its Interfaces block names.
- Every internal package must be reached by an entry point (`cmd/curia` or the e2e test through the binary) by the end of the plan.
- Enforcement: `make check` runs `deadcode -test ./...` during the plan (decision 0004). The Wave 5 done criterion in PROGRESS.md runs `deadcode ./...` without `-test` on `plan-1`; every reported symbol must be a deliberate test double (`tmux.Fake`) or be fixed before the PR to main.

## Consequences

- Packages exist for several waves before any command uses them. Their interfaces come from the plan, so a consumer that needs a different shape raises it with the orchestrator rather than changing a lower package in passing.
- A function that only tests call can survive until Wave 5; the Wave 5 check catches it.
