#!/usr/bin/env bash
# Run a command while holding a machine-wide lock shared by every worktree of
# this repository, so that several worktrees running `make check` at once do
# not run their test suites at the same time. The suites contain latency
# gates; running them concurrently measures the other suite, not the code.
# The command and its thresholds are unchanged. The lock lives in the git
# common directory; a lock whose owner process is gone is taken over.
set -euo pipefail

common=$(git rev-parse --git-common-dir 2>/dev/null) || exec "$@"
lock="$common/curia-test.lock"
waited=0
until mkdir "$lock" 2>/dev/null; do
	owner=$(cat "$lock/pid" 2>/dev/null || true)
	if [ -n "$owner" ] && ! kill -0 "$owner" 2>/dev/null; then
		rm -rf "$lock"
		continue
	fi
	if [ "$waited" -eq 0 ]; then
		echo "waiting for another worktree's test run (pid ${owner:-?})..." >&2
	fi
	sleep 2
	waited=$((waited + 2))
	if [ "$waited" -ge 1800 ]; then
		echo "test lock held for 30 minutes by pid ${owner:-?}; giving up" >&2
		exit 1
	fi
done
echo $$ >"$lock/pid"
trap 'rm -rf "$lock"' EXIT
"$@"
