# 0005. lefthook and the conventional-commit check

Status: accepted (Phase 1 setup)

## Context

The owner requires small commits with conventional-commit messages and a pre-commit gate identical to CI. Hooks must be versioned in the repository and need no Node toolchain.

## Decision

- lefthook (a single Go binary; installed via Homebrew on the dev Mac) manages hooks from `lefthook.yml`. Each clone or worktree runs `lefthook install` once.
- `pre-commit` runs exactly `make check` (decision 0007). It checks the working tree, not only the staged files, so commit with a clean tree.
- `commit-msg` runs `scripts/check-commit-msg.sh <msgfile>`, a bash script with no dependencies. It accepts `<type>[(scope)][!]: <description>` with type in `feat, fix, docs, style, refactor, perf, test, build, ci, chore, revert`, a lowercase scope, and a subject of at most 72 characters. Git's own `Merge ...` and `Revert "..."` subjects pass. Comment lines are ignored.
- Hooks are never bypassed (`--no-verify` is forbidden by AGENTS.md).

## Consequences

- Verified during setup: a non-conventional message is rejected by the real hook, a conventional one passes.
- The hook needs Go and `make` on PATH in the committing shell (AGENTS.md).
- lefthook is a dev-machine prerequisite; CI does not use it, it runs `make check` directly.
