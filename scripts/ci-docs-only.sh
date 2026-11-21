#!/usr/bin/env bash
# Reads changed file names (one per line) on stdin; prints "true" only if EVERY
# one is documentation, else "false". CI's `plan` job uses it to skip the test
# jobs (docs/decisions/0007, owner decision 2026-11-20).
# Docs are: **/*.md, docs/**, LICENSE. Never docs, even if they match: anything
# under .github/ or scripts/, any testdata/ path, and any file a go:embed
# directive in the repo can pick up (the Go tests read those). An empty list or
# any doubt prints "false": unknown means run everything.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)" || { echo false; exit 0; }
# "dir pattern" per go:embed pattern, relative to the repo root.
embeds=$(git grep -n '^//go:embed ' -- '*.go' || true)
n=0
while IFS= read -r f; do
	[ -n "$f" ] || continue
	n=$((n + 1))
	case $f in
	.github/* | scripts/* | testdata/* | */testdata/*) echo false; exit 0 ;;
	*.md | docs/* | LICENSE) ;;
	*) echo false; exit 0 ;;
	esac
	while IFS= read -r line; do
		[ -n "$line" ] || continue
		dir=$(dirname "${line%%:*}")
		for pat in ${line#*//go:embed }; do
			pat=${pat#all:}
			# shellcheck disable=SC2254
			case $f in $dir/$pat | $dir/$pat/*) echo false; exit 0 ;; esac
		done
	done <<<"$embeds"
done
[ "$n" -gt 0 ] && echo true || echo false
