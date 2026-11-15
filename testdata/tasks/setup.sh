#!/bin/sh
# The acceptance test's .curia/setup.sh: it records that it ran, for which
# task and where, outside the worktree.
printf '%s %s\n' "$CURIA_TASK_ID" "$PWD" >>"$HOME/setup-ran"
