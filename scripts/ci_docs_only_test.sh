#!/bin/sh
# Table test of ci-docs-only.sh: changed-file lists to the docs_only decision.
set -u
script=$(cd "$(dirname "$0")" && pwd)/ci-docs-only.sh
fails=0
# expect WANT FILE...
expect() {
	want=$1; shift
	got=$(printf '%s\n' "$@" | "$script")
	[ "$got" = "$want" ] || { echo "FAIL: '$*' gave $got, want $want" >&2; fails=1; }
}
expect true README.md
expect true docs/decisions/0007-x.md docs/img/a.png AGENTS.md PROGRESS.md
expect true LICENSE sub/dir/NOTES.md
expect false README.md main.go
expect false go.mod
expect false go.sum
expect false Makefile
expect false scripts/shard.sh
expect false scripts/README.md
expect false .github/workflows/ci.yml
expect false .github/pull_request_template.md
expect false .golangci.yml
expect false cmd/curia/testdata/golden.md
expect false testdata/x.md
expect false internal/install/skill/SKILL.md
expect false internal/store/migrations/001.sql
expect false README.md notes.txt
expect false LICENSE.txt
expect false
echo "docs-only table: done"
exit $fails
