#!/usr/bin/env bash
# Every test that asserts a latency percentile must be recognisable by name,
# because `make test` runs the latency gates alone in a second step and
# selects them by name ("Latency" in the name, or TestReplayFiveSessions).
# A percentile assertion in a test with another name would run beside the
# other packages and measure their load. This check finds test functions
# whose body mentions p95 and whose name the selector would miss.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
bad=0
while IFS= read -r f; do
	# Print "file:TestName" for each test function whose body mentions p95.
	awk -v file="$f" '
		/^func Test[A-Za-z0-9_]*\(/ { name=$2; sub(/\(.*/, "", name); seen=0 }
		/p95/ { if (name != "" && !seen) { print file ":" name; seen=1 } }
		/^}/ { name="" }
	' "$f"
done < <(git ls-files '*_test.go') | while IFS=: read -r file name; do
	case "$name" in
	*Latency* | TestReplayFiveSessions) ;;
	*)
		echo "latency gate with a name the test step would not run alone: $file: $name" >&2
		echo "rename it to contain \"Latency\"" >&2
		exit 1
		;;
	esac
done || bad=1
exit "$bad"
