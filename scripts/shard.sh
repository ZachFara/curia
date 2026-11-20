#!/usr/bin/env bash
# CI-only sharding of the slow test steps; `make check` never uses it.
#   shard.sh list latency   packages that hold a latency gate (matching $LATENCY_TESTS), sorted
#   shard.sh list cmd       top-level tests of ./cmd/curia that are not latency gates, sorted
#   shard.sh pick I N       from stdin, the lines at sorted position p with p mod N == I
# Rule: sort (C locale), then item p goes to shard p mod N. Computed from the
# code, never a hand-kept list, so a new test or package lands in a shard.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
case "${1:-}" in
list)
	pat=${LATENCY_TESTS:?LATENCY_TESTS must be set (the Makefile exports it)}
	case "${2:-}" in
	latency) go test -list "$pat" ./... | awk '/^ok/ { if (has) print $2; has = 0; next } /^(Test|Example|Fuzz)/ { has = 1 }' ;;
	cmd) go test -list . ./cmd/curia | grep -E '^(Test|Example|Fuzz)' | grep -vE "$pat" || true ;;
	*) echo "usage: shard.sh list latency|cmd" >&2; exit 2 ;;
	esac | LC_ALL=C sort
	;;
pick)
	out=$(LC_ALL=C sort | awk -v i="${2:?}" -v n="${3:?}" '(NR - 1) % n == i')
	[ -n "$out" ] || { echo "shard $2 of $3 is empty: too many shards for the list" >&2; exit 1; }
	echo "$out"
	;;
*) echo "usage: shard.sh list|pick" >&2; exit 2 ;;
esac
