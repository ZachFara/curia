#!/bin/sh
# Self-check of the path-to-package mapping in staged-tests.sh --list.
set -eu
script=$(cd "$(dirname "$0")" && pwd)/staged-tests.sh
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cd "$tmp"
mkdir -p internal/a/testdata/x/y "dir with space" gone
touch main.go internal/a/a.go internal/a/testdata/x/y/f.txt "dir with space/s.go" "internal/a/my file.go" README.md
fails=0
# expect WANT PATH...
expect() {
	want=$1; shift
	got=$(printf '%s\n' "$@" | sh "$script" --list | tr '\n' ' ')
	if [ "$got" != "$want" ]; then echo "FAIL: $* -> '$got', want '$want'" >&2; fails=1; fi
}
expect "./internal/a " internal/a/a.go internal/a/testdata/x/y/f.txt
expect "./internal/a " "internal/a/my file.go"
expect ". " main.go
expect "" gone/deleted.go
expect "" README.md
expect "" "dir with space/s.go"
expect "./... " go.mod internal/a/a.go
MAX_PKGS=1 expect "./... " main.go internal/a/a.go
exit $fails
