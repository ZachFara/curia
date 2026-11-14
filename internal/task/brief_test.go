package task

import (
	"strings"
	"testing"

	"github.com/ZachFara/curia/internal/model"
)

const briefHead = `You are working on Curia task T-42 in your own git worktree, on branch curia/T-42-fix-auth (from main). Other agents work in other worktrees: stay inside this one.

Rules for this session:
- When you face a judgment call the user should make or confirm, use the curia MCP tools: call search_decisions first, then request_decision with options and your recommendation. Use mode "blocking" only for choices that are costly or impossible to undo.
- Messages from the user may arrive while you work, as "Message M-<n> from the user". Follow them. If one says its wording was simplified, call check_messages.
`

const teamLine = "- You may form an agent team if the task splits into independent parts. You are the lead: teammates bring their questions to you, and only you raise Curia decision tickets.\n"

const briefTail = `- Commit your work on this branch as you go. Do not push, merge or open a pull request unless the task says so.
- When the task is finished, make sure everything is committed, then end your turn with a short summary: what you changed, how you tested it, and anything left open.

Task T-42: Fix auth

`

func bt(body string) model.Task {
	return model.Task{ID: "T-42", Title: "Fix auth", Body: body, Branch: "curia/T-42-fix-auth", BaseBranch: "main"}
}

func TestBriefWording(t *testing.T) {
	if got, want := Brief(bt("Do it."), false), briefHead+briefTail+"Do it."; got != want {
		t.Errorf("solo:\n%s", got)
	}
	if got, want := Brief(bt("Do it."), true), briefHead+teamLine+briefTail+"Do it."; got != want {
		t.Errorf("team:\n%s", got)
	}
}

func TestBriefKeepsBody(t *testing.T) {
	body := "run $(x) and \"quotes\" 'q'\n;\n`tick`"
	if got := Brief(bt(body), true); !strings.HasSuffix(got, "\n\n"+body) {
		t.Errorf("body altered: %q", got)
	}
}
