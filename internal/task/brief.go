package task

import (
	"strings"

	"github.com/ZachFara/curia/internal/model"
)

const teamRule = "- You may form an agent team if the task splits into independent parts. You are the lead: teammates bring their questions to you, and only you raise Curia decision tickets.\n"

// Brief is the first prompt of t's agent (section 4.6); team adds the team
// rule.
func Brief(t model.Task, team bool) string {
	var b strings.Builder
	b.WriteString("You are working on Curia task " + t.ID + " in your own git worktree, on branch " + t.Branch + " (from " + t.BaseBranch + "). Other agents work in other worktrees: stay inside this one.\n\n")
	b.WriteString("Rules for this session:\n")
	b.WriteString("- When you face a judgment call the user should make or confirm, use the curia MCP tools: call search_decisions first, then request_decision with options and your recommendation. Use mode \"blocking\" only for choices that are costly or impossible to undo.\n")
	b.WriteString("- Messages from the user may arrive while you work, as \"Message M-<n> from the user\". Follow them. If one says its wording was simplified, call check_messages.\n")
	if team {
		b.WriteString(teamRule)
	}
	b.WriteString("- Commit your work on this branch as you go. Do not push, merge or open a pull request unless the task says so.\n")
	b.WriteString("- When the task is finished, make sure everything is committed, then end your turn with a short summary: what you changed, how you tested it, and anything left open.\n\n")
	b.WriteString("Task " + t.ID + ": " + t.Title + "\n\n")
	b.WriteString(t.Body)
	return b.String()
}
