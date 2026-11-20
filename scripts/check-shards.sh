#!/usr/bin/env bash
# Fails if the CI shards (scripts/shard.sh) do not partition their set: every
# latency package and every cmd/curia test must be in exactly one shard, and
# the matrices in ci.yml must list exactly the shard indexes 0..N-1.
# Needs LATENCY_TESTS, LATENCY_SHARDS and CMD_SHARDS (the Makefile exports them).
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
	want="[$(seq 0 $((n - 1)) | paste -sd, - | sed 's/,/, /g')]"
	have=$(grep "# ${name}-shards" .github/workflows/ci.yml | grep -o '\[.*\]' || true)
	if [ "$have" != "$want" ]; then
		echo "ci.yml matrix for $name is '$have', want '$want'"
		bad=1
	fi
	echo "$name: $(echo "$full" | wc -l | tr -d ' ') items in $n shards, partition ok"
done
exit $bad
