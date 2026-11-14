# 0019. Teams are read from Claude Code's config

Status: accepted (Plan 3 setup). Probe P5 is recorded as a Ruling from 97 in `PROGRESS.md`; the file's exact shape is recorded here once it reports.

## Context

Spec F14 shows an Agent Team as one unit and lets only its lead raise tickets. No team has ever run on this machine, so the config's location and shape, and which field ties a member to a hook `session_id` or a pane, are unverified until probe P5.

## Decision

- **Membership is read from `<Claude directory>/teams/<team>/config.json`** (`install.ClaudeDir`: `$CLAUDE_CONFIG_DIR` or `~/.claude`) when a view or `curia mcp` refreshes, never in `curia hook` or `curia status` (a directory scan has no place in a 20 ms budget). It is read at the end of `reconcile`, in `curia say` before it builds candidates, and in `curia mcp` before it resolves the caller of `request_decision`, so the lead-only rule never lags.
- **Reading fails soft:** at most 64 team directories, in name order; each `config.json` a regular file of at most 1 MiB opened without blocking; one that fails is skipped and logged once at WARN; names pass `sanitize.Line(..., 64)` and pane ids must match `^%[0-9]+$`. Every session then shows as solo.
- **`team.Assign`** (pure) matches a member to the live session whose id is the member's session id when the config gives one, else the live session whose pane is the member's pane; the lead also matches `leadSessionId`. A team with no matched lead assigns nobody.
- **`store.SetTeams`** stores `team_id`, `team_role` and `team_member` in one write transaction, only when they differ, without touching `updated_at` or `seen_at`, and gives a teammate with no task its lead's (decision 39 of the plan).
- **Lead-only:** `request_decision` from a caller whose `team_role` is `teammate` returns the tool error `ask_lead`. The other tools work for a teammate.
- **Dashboard:** `fleet.Units` groups a lead with its teammates, ranked by the worst member; teams are collapsed by default. Cost and cache state on the group row wait for Plan 5, per-team cost in the review for Plan 4.
- The plan's assumed shape (until P5): top-level `leadSessionId` and a `members` array whose entries carry `name`, `agentType` (`team-lead` for the lead), `tmuxPaneId` and `cwd`.

## Consequences

- Membership can lag a view by one refresh; the MCP server refreshes on every `request_decision`.
- If P5 finds no pane or session id in the config, `team.Assign` matches teammates as the live sessions whose pane is in the lead's tmux window and that started after the lead. If no switch puts teammates in tmux panes, `--team` sets only the environment variable and the dashboard shows the lead alone. If there is no config at all, F14's detection is dropped from this plan and the lead-only rule waits. Which of these happened is dated here when P5 reports.
