#!/bin/sh
# CI only (owner decision D-15, docs/decisions/0007): run a command; if it
# fails, run the same command once more (the whole command, not only the
# gates that failed). The job passes if the second run passes. When the rerun
# clears a first failure, say so loudly: a ::warning:: annotation and a job
# summary with the first run's verdict lines. No third attempt.
set -u
a=$(mktemp) b=$(mktemp)
trap 'rm -f "$a" "$b"' EXIT
verdicts() { grep -E 'batch p95.*: FAIL' "$1" || echo "(no verdict line; see the log)"; }
summary() { [ -z "${GITHUB_STEP_SUMMARY:-}" ] || printf '%s\n' "$@" >>"$GITHUB_STEP_SUMMARY"; }

"$@" >"$a" 2>&1 && { cat "$a"; exit 0; }
cat "$a"
echo "==> first run failed; running the same command once more: $*"
"$@" >"$b" 2>&1 && rc=0 || rc=$?
cat "$b"
if [ "$rc" -eq 0 ]; then
	echo "::warning::latency flake: the first run failed and the rerun passed ($*); see the job summary for the first run's verdicts"
	summary "### Latency flake: first run failed, rerun passed" '```' "$(verdicts "$a")" '```'
else
	echo "::error::the first run and the rerun both failed ($*)"
	summary "### Latency gate failed twice" "First run:" '```' "$(verdicts "$a")" '```' "Rerun:" '```' "$(verdicts "$b")" '```'
fi
exit "$rc"
