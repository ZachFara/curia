#!/bin/sh
# Pre-commit test step: race-test only the Go packages the staged files touch.
#   staged-tests.sh          run the tests (reads `git diff --cached`)
#   staged-tests.sh --list   print the packages for staged paths read from stdin
# A staged .go file maps to its directory; a file under a testdata/ directory
# (any depth) maps to the directory holding that testdata/. go.mod or go.sum
# staged, or more than MAX_PKGS packages, means the whole module (./...).
# Latency tests never run here; CI runs them (docs/decisions/0007).
# shortcut: a directory with a space is skipped (not a valid Go package path).
set -eu

MAX_PKGS=${MAX_PKGS:-20}

map() {
	while IFS= read -r p; do
		case "$p" in
		go.mod|go.sum) echo './...'; continue ;;
		testdata/*) d=. ;;
		*/testdata/*) d=${p%%/testdata/*} ;;
		*.go) d=$(dirname "$p") ;;
		*) continue ;;
		esac
		case "$d" in *' '*) continue ;; esac
		# A deleted package or a dir with no Go files has nothing to test.
		ls "$d"/*.go >/dev/null 2>&1 || continue
		if [ "$d" = . ]; then echo .; else echo "./$d"; fi
	done | sort -u
}

list() {
	pkgs=$(map)
	if [ -z "$pkgs" ]; then return 0; fi
	if printf '%s\n' "$pkgs" | grep -qx './\.\.\.' ||
		[ "$(printf '%s\n' "$pkgs" | wc -l)" -gt "$MAX_PKGS" ]; then
		echo ./...
	else
		printf '%s\n' "$pkgs"
	fi
}

if [ "${1:-}" = --list ]; then
	list
	exit 0
fi

pkgs=$(git -c core.quotepath=off diff --cached --name-only --diff-filter=ACMR | list)
if [ -z "$pkgs" ]; then
	echo "==> no staged Go or testdata files: no tests"
	exit 0
fi
echo "==> go test -race (staged packages): $(echo $pkgs)"
# shellcheck disable=SC2086 # package paths hold no spaces (filtered above)
go test -race -skip 'Latency|TestReplayFiveSessions' $pkgs
