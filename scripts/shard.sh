#!/usr/bin/env bash
# CI-only sharding of the slow test steps; `make check` never uses it.
#   shard.sh list latency   "package test" lines, one per latency gate (matching $LATENCY_TESTS), sorted
#   shard.sh list rest      packages of job unit-rest: all but cmd/curia and $STORE_PKG, sorted
#   shard.sh list all       every package (go list ./...), sorted
#   shard.sh list cmd       top-level tests of ./cmd/curia that are not latency gates, sorted
#   shard.sh pick I N       from stdin, the lines at sorted position p with p mod N == I
#   shard.sh pickw I N      from stdin "pkg test" lines, shard I of N when each test, heaviest first,
#                           goes to the lightest shard (weights: scripts/latency-weights.txt, seconds
#                           per pass; a test with no weight counts as 15)
# Rule: sort (C locale), then item p goes to shard p mod N. Computed from the
# code, never a hand-kept list, so a new test or package lands in a shard.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
case "${1:-}" in
list)
	pat=${LATENCY_TESTS:?LATENCY_TESTS must be set (the Makefile exports it)}
	case "${2:-}" in
	latency) go test -list "$pat" ./... | awk '/^ok/ { for (i = 0; i < n; i++) print $2, t[i]; n = 0; next } /^(Test|Example|Fuzz)/ { t[n++] = $1 }' ;;
	rest) go list ./... | grep -vxF -e "$(go list ./cmd/curia)" -e "$(go list "${STORE_PKG:?STORE_PKG must be set}")" ;;
	all) go list ./... ;;
	cmd) go test -list . ./cmd/curia | grep -E '^(Test|Example|Fuzz)' | grep -vE "$pat" || true ;;
	*) echo "usage: shard.sh list latency|cmd|rest|all" >&2; exit 2 ;;
	esac | LC_ALL=C sort
	;;
pick)
	out=$(LC_ALL=C sort | awk -v i="${2:?}" -v n="${3:?}" '(NR - 1) % n == i')
	[ -n "$out" ] || { echo "shard $2 of $3 is empty: too many shards for the list" >&2; exit 1; }
	echo "$out"
	;;
pickw)
	out=$(LC_ALL=C sort | awk -v i="${2:?}" -v n="${3:?}" -v wf="$(dirname "$0")/latency-weights.txt" '
		BEGIN { while ((getline l < wf) > 0) if (l !~ /^#/ && split(l, f, " ") == 2) w[f[1]] = f[2] }
		{ line[NR] = $0; wt[NR] = ($2 in w) ? w[$2] : 15 }
		END {
			for (k = 1; k <= NR; k++) { # heaviest first, ties in sorted order
				best = 0
				for (j = 1; j <= NR; j++) if (!used[j] && (best == 0 || wt[j] > wt[best])) best = j
				used[best] = 1; lo = 0
				for (s = 1; s < n; s++) if (load[s] < load[lo]) lo = s
				load[lo] += wt[best]
				if (lo == i) print line[best]
			}
		}')
	[ -n "$out" ] || { echo "shard $2 of $3 is empty: too many shards for the list" >&2; exit 1; }
	echo "$out"
	;;
group)
	# "pkg test" lines to "pkg ^(t1|t2)$" lines: one go test run per package.
	awk '{ a[$1] = a[$1] (a[$1] ? "|" : "") $2 } END { for (p in a) print p, "^(" a[p] ")$" }' | LC_ALL=C sort
	;;
*) echo "usage: shard.sh list|pick|pickw|group" >&2; exit 2 ;;
esac
