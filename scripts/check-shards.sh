#!/usr/bin/env bash
# Fails if the CI shards (scripts/shard.sh) do not partition their set: every
# latency gate (package + test) and every cmd/curia test must be in exactly one shard, and
# ci.yml must take its matrices from the Makefile's counts, and the unit jobs
# must cover every package.
# Needs LATENCY_TESTS, LATENCY_SHARDS, CMD_SHARDS and STORE_PKG (the Makefile exports them).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
bad=0
for kind in latency:"$LATENCY_SHARDS" cmd:"$CMD_SHARDS"; do
	name=${kind%%:*}
	n=${kind##*:}
	full=$(scripts/shard.sh list "$name")
	[ -n "$full" ] || { echo "no $name items found"; bad=1; continue; }
	got=$(for ((i = 0; i < n; i++)); do printf '%s\n' "$full" | scripts/shard.sh pick "$i" "$n"; done | LC_ALL=C sort)
	if [ "$got" != "$full" ]; then
		echo "$name shards are not a partition of the set (missing or duplicated items):"
		diff <(echo "$full") <(echo "$got") || true
		bad=1
	fi
	# The matrix comes from `make shard-matrices` (the Makefile's count), never a literal list.
	grep -q "fromJSON(needs.plan.outputs.$name)" .github/workflows/ci.yml || { echo "ci.yml's $name matrix must be fromJSON(needs.plan.outputs.$name)"; bad=1; }
	echo "$name: $(echo "$full" | wc -l | tr -d ' ') items in $n shards, partition ok"
done
# unit-store, unit-rest and cmd/curia together are every package, each once.
all=$(scripts/shard.sh list all | LC_ALL=C sort)
got=$({ go list ./cmd/curia; go list "$STORE_PKG"; scripts/shard.sh list rest; } | LC_ALL=C sort)
if [ "$got" != "$all" ]; then
	echo "unit-store + unit-rest + cmd/curia are not exactly go list ./...:"
	diff <(echo "$all") <(echo "$got") || true
	bad=1
fi
echo "packages: $(echo "$all" | wc -l | tr -d ' ') = cmd/curia + $STORE_PKG + unit-rest, ok"
exit $bad
