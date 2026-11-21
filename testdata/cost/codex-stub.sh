#!/bin/sh
# Stands in for Codex in e2e/cost_test.go. The test links it into a directory
# first on PATH as `codex`, next to `curia`. It records its arguments (one per
# line), its directory and its task in that directory, reports its start
# through `curia hook SessionStart --agent codex` as the hooks curia install
# --codex registers would, draws nothing and waits for its input to end.
dir=$(dirname "$0")
{
	printf 'argc %d\n' "$#"
	for a in "$@"; do printf 'arg %s\n' "$a"; done
	printf 'cwd %s\ntask %s\n' "$PWD" "$CURIA_TASK_ID"
} >>"$dir/codex-argv-$CURIA_TASK_ID.txt"

# The session id is derived from the task id (T-7 is 00000007-...-000000000007).
n=${CURIA_TASK_ID#T-}
id=$(printf '%08d-0000-4000-9000-%012d' "$n" "$n")

hook() {
	printf '%s' "$2" | curia hook "$1" --agent codex >/dev/null 2>&1
}

hook SessionStart "{\"session_id\":\"$id\",\"cwd\":\"$PWD\",\"hook_event_name\":\"SessionStart\",\"source\":\"startup\"}"
while IFS= read -r line; do :; done
hook SessionEnd "{\"session_id\":\"$id\",\"cwd\":\"$PWD\",\"hook_event_name\":\"SessionEnd\",\"reason\":\"other\"}"
