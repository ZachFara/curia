# 0004. Dead-code detection

Status: accepted (Phase 1 setup)

## Context

`unused` (golangci-lint) only finds unexported dead code. Exported functions that no entry point reaches are invisible to it, and in a bottom-up build (decision 0008) they are the main risk: a package built for a later consumer that never arrives. `golang.org/x/tools/cmd/deadcode` finds functions unreachable from any `main`, using whole-program analysis.

## Decision

- `make deadcode` runs `deadcode -test ./...` and fails if it prints anything (deadcode itself exits 0 on findings).
- `-test` makes test binaries count as roots. Reason: Plan 1 is built bottom-up in parallel waves, so packages like `store` exist for several waves before any command reaches them. Without `-test`, every wave-2 PR would fail.
- Cost if wrong: a function used only by tests survives until the end of the plan. Mitigation, a Wave 5 done criterion in PROGRESS.md: the orchestrator runs `deadcode ./...` (no `-test`) on `plan-1`, and every reported symbol is either a deliberate test double (`tmux.Fake`) or fixed before the PR to main.

## Consequences

- During Waves 1 to 4, a function is "alive" if a test calls it. Reviewers should still question exported functions that only tests use.
- The no-`-test` run is a manual orchestrator step, not part of `make check`, because `tmux.Fake` is legitimately reachable only from tests.
