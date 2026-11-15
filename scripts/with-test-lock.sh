#!/usr/bin/env bash
# Run a command while holding a machine-wide lock shared by every worktree of
# this repository, so that several worktrees running `make check` at once do
# not run their gates at the same time. The test suites contain latency
# gates; another worktree's linters, builds or tests running beside them
# make those gates measure the machine's contention, not the code.
# The command and its thresholds are unchanged. The lock lives in the git
# common directory; a lock whose owner process is gone is taken over.
#
# Waiters are served in the order they arrived: each one drops a ticket named
# after its arrival time into a queue directory beside the lock and takes the
# lock only while its ticket is the oldest one whose process is still alive.
# Without that, a newcomer could take the lock ahead of a run that had been
# waiting for half an hour.
set -euo pipefail

# Re-entrant: `make check` holds the lock for all gates, and its test step
# calls this script again.
if [ -n "${CURIA_TEST_LOCK_HELD:-}" ]; then
	exec "$@"
fi
common=$(git rev-parse --git-common-dir 2>/dev/null) || exec "$@"
lock="$common/curia-test.lock"
queue="$common/curia-test.queue"
mkdir -p "$queue"
ticket="$queue/$(date +%s)-$(printf '%07d' $$)"
echo $$ >"$ticket"
held=0
cleanup() {
	rm -f "$ticket"
	if [ "$held" -eq 1 ]; then
		rm -rf "$lock"
	fi
}
trap cleanup EXIT

# first_in_line succeeds when this process's ticket is the oldest one whose
# owner is alive. Tickets of dead processes are removed on the way.
first_in_line() {
	local t owner
	for t in "$queue"/*; do
		[ -e "$t" ] || continue
		if [ "$t" = "$ticket" ]; then
			return 0
		fi
		owner=$(cat "$t" 2>/dev/null || true)
		if [ -n "$owner" ] && kill -0 "$owner" 2>/dev/null; then
			return 1
		fi
		rm -f "$t"
	done
	return 0
}

waited=0
while :; do
	if first_in_line && mkdir "$lock" 2>/dev/null; then
		break
	fi
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
	if [ "$waited" -ge 5400 ]; then
		echo "waited 90 minutes for the test lock (held by pid ${owner:-?}); giving up" >&2
		exit 1
	fi
done
held=1
echo $$ >"$lock/pid"
CURIA_TEST_LOCK_HELD=1 "$@"
