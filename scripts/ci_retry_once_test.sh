#!/bin/sh
# Self-check of ci-retry-once.sh: fails once then passes, fails twice, passes first time.
set -eu
script=$(cd "$(dirname "$0")" && pwd)/ci-retry-once.sh
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
# fake COUNT_FILE FAILS: fails its first FAILS runs, printing a verdict line each time.
cat >"$tmp/fake" <<'F'
#!/bin/sh
n=$(cat "$1" 2>/dev/null || echo 0); n=$((n+1)); echo $n >"$1"
echo "gate$n: batch p95 [30ms] at [x], median 30ms: FAIL too slow"
[ "$n" -gt "$2" ]
F
chmod +x "$tmp/fake"
fails=0
# expect NAME FAILS WANT_RC WANT_RUNS WANT_WARNING
expect() {
	rm -f "$tmp/n" "$tmp/sum"
	rc=0; out=$(GITHUB_STEP_SUMMARY=$tmp/sum sh "$script" "$tmp/fake" "$tmp/n" "$2") || rc=$?
	runs=$(cat "$tmp/n"); warn=0; case $out in *::warning::*) warn=1 ;; esac
	[ "$rc $runs $warn" = "$3 $4 $5" ] || { echo "FAIL $1: rc/runs/warning '$rc $runs $warn', want '$3 $4 $5'" >&2; fails=1; }
	echo "--- $1: rc=$rc runs=$runs"; printf '%s\n' "$out"; [ ! -f "$tmp/sum" ] || cat "$tmp/sum"
}
expect "passes first time" 0 0 1 0
expect "fails once then passes" 1 0 2 1
expect "fails twice" 2 1 2 0
grep -q gate1 "$tmp/sum" && grep -q gate2 "$tmp/sum" || { echo "FAIL: summary lacks both tables" >&2; fails=1; }
exit $fails
