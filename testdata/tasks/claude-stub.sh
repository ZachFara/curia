#!/bin/sh
# Stands in for Claude Code in e2e/tasks_test.go. The test links it into a
# directory first on PATH as `claude`, next to `curia`, screen.txt (Claude
# Code's empty prompt) and router-envelope.json; everything it records goes
# into that directory too, so it needs no variable of its own.
dir=$(dirname "$0")

# Called with -p it is the router: it records its arguments, environment and
# prompt, answers with the recorded envelope and exits.
if [ "$1" = "-p" ]; then
	{
		printf 'argc %d\n' "$#"
		for a in "$@"; do printf 'arg %s\n' "$a"; done
		env | sort | sed 's/^/env /'
		printf 'end\n'
	} >>"$dir/router.txt"
	cat >"$dir/router-prompt.txt"
	cat "$dir/router-envelope.json"
	exit 0
fi

# The agent: it records its arguments (one per line), its directory and its
# task, then reports as Claude Code does.
{
	printf 'argc %d\n' "$#"
	for a in "$@"; do printf 'arg %s\n' "$a"; done
	printf 'cwd %s\ntask %s\n' "$PWD" "$CURIA_TASK_ID"
} >>"$dir/argv-$CURIA_TASK_ID.txt"

# The session id is derived from the task id (T-1 is 00000001-...-000000000001),
# or is the one after --resume.
n=${CURIA_TASK_ID#T-}
id=$(printf '%08d-0000-4000-8000-%012d' "$n" "$n")
source=startup
prev=
for a in "$@"; do
	if [ "$prev" = --resume ]; then
		id=$a
		source=resume
	fi
	prev=$a
done

hook() {
	printf '%s' "$2" | curia hook "$1" >/dev/null 2>&1
}

draw() {
	printf '\033[H\033[2J'
	cat "$dir/screen.txt"
}

hook SessionStart "{\"session_id\":\"$id\",\"cwd\":\"$PWD\",\"hook_event_name\":\"SessionStart\",\"source\":\"$source\"}"
draw
# Each line typed into the prompt is submitted: UserPromptSubmit with the line
# as the prompt, then the turn's Stop. The prompt is drawn again, and only
# then is the line recorded, so the test reads it once the turn is over.
while IFS= read -r line; do
	prompt=$(printf '%s' "$line" | sed 's/\\/\\\\/g; s/"/\\"/g')
	hook UserPromptSubmit "{\"session_id\":\"$id\",\"cwd\":\"$PWD\",\"permission_mode\":\"default\",\"hook_event_name\":\"UserPromptSubmit\",\"prompt\":\"$prompt\"}"
	hook Stop "{\"session_id\":\"$id\",\"cwd\":\"$PWD\",\"permission_mode\":\"default\",\"hook_event_name\":\"Stop\",\"stop_hook_active\":false,\"last_assistant_message\":\"Done.\"}"
	draw
	printf '%s\n' "$line" >>"$dir/input-$CURIA_TASK_ID.txt"
done
# End of input (the user left the agent): Claude Code's SessionEnd.
hook SessionEnd "{\"session_id\":\"$id\",\"cwd\":\"$PWD\",\"hook_event_name\":\"SessionEnd\",\"reason\":\"prompt_input_exit\"}"
