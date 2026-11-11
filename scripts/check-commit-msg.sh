#!/usr/bin/env bash
# Conventional-commit check (docs/decisions/0005), run by the lefthook
# commit-msg hook with the path to the commit message file.
#
# Accepted subject: <type>[(<scope>)][!]: <description>, at most 72 characters,
# where type is one of the list below. Git's own merge and revert subjects are
# also accepted. Comment lines (starting with #) are ignored.
set -euo pipefail

if (($# != 1)); then
	echo "usage: $0 <commit-message-file>" >&2
	exit 2
fi

subject=$(grep -v '^#' "$1" | sed -e '/^[[:space:]]*$/d' | head -n 1 || true)

if [[ -z $subject ]]; then
	echo "commit-msg: empty commit message" >&2
	exit 1
fi

if [[ $subject =~ ^Merge\  || $subject =~ ^Revert\ \" ]]; then
	exit 0
fi

types='feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert'
pattern="^(${types})(\([a-z0-9][a-z0-9._/-]*\))?!?: [^[:space:]].*$"

if [[ ! $subject =~ $pattern ]]; then
	cat >&2 <<EOF
commit-msg: subject is not a conventional commit:
  $subject
expected: <type>[(scope)][!]: <description>
types: ${types//|/, }
EOF
	exit 1
fi

if ((${#subject} > 72)); then
	echo "commit-msg: subject is ${#subject} characters; the limit is 72" >&2
	exit 1
fi
