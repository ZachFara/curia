#!/bin/sh
# Stands in for node in e2e/cost_test.go, so the cost-lens analyzer never
# runs. The test links it into a directory first on PATH as `node`, next to
# summary.json (a cost-lens analyzer summary); it records its arguments, one
# per line, in that directory, prints the summary and exits.
dir=$(dirname "$0")
{
	printf 'argc %d\n' "$#"
	for a in "$@"; do printf 'arg %s\n' "$a"; done
} >>"$dir/node-argv.txt"
cat "$dir/summary.json"
