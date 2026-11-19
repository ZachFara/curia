package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/task"
)

// complete_task's fixed texts and schema (plan section 4.3).
const (
	completeName        = "complete_task"
	completeDescription = "Tell the user the task is finished and ready for review. Call it once everything is committed, with a short summary and the state of the tests."
	// completeSchema states the limits complete_task checks (task.MaxSummary,
	// task.MaxTestsDetail, in characters), so an agent need not learn them from
	// a refusal (live check defect 4; TestCompleteSchemaStatesLimits).
	completeSchema = `{"type":"object","required":["summary","tests_status"],"properties":{"summary":{"type":"string","maxLength":2000},"tests_status":{"type":"string","enum":["passed","failed","not_run"]},"tests_detail":{"type":"string","maxLength":200}}}`

	instrComplete = "The review is waiting for the user. Make sure everything is committed, then end your turn. Do not merge or push. If the user asks for changes, their note arrives as a message."
	msgDoneLead   = "You are a teammate in an agent team. Tell your team lead you are done; only the lead completes the task."
	msgNoTask     = "This session is not linked to a Curia task, so there is nothing to complete. End your turn with a summary instead."

	// maxTaskIDEcho bounds the caller's task id where an error repeats it.
	maxTaskIDEcho = 16
)

// completeTool is the sixth tool.
func (t *tools) completeTool() Tool {
	return Tool{
		Name: completeName, Description: completeDescription,
		InputSchema: json.RawMessage(completeSchema), Call: t.wrap(completeName, t.complete),
	}
}

type completeArgs struct {
	Summary     string `json:"summary"`
	TestsStatus string `json:"tests_status"`
	TestsDetail string `json:"tests_detail"`
}

// completeResult is complete_task's result.
type completeResult struct {
	Review      string `json:"review"`
	Task        string `json:"task"`
	Updated     bool   `json:"updated"`
	Instruction string `json:"instruction"`
}

// complete is complete_task: the caller says its own task is ready for
// review. It opens the task's pending review, or updates the one already
// pending, with the agent's summary and its own report of the tests, and
// nothing else: it approves, merges, closes and drops nothing, changes no
// stored task status, reads no file and runs no program. The task is
// always the caller's (from its session row); no argument names one. A
// teammate is refused before its arguments are read, then a caller with no
// session or no task.
func (t *tools) complete(ctx context.Context, args json.RawMessage, now time.Time) (any, *ToolError) {
	c := t.Caller(ctx)
	if c.TeamRole == model.RoleTeammate {
		return nil, &ToolError{Code: "ask_lead", Message: msgDoneLead}
	}
	if c.SessionID == "" || c.TaskID == "" {
		return nil, &ToolError{Code: "no_task", Message: msgNoTask}
	}
	var a completeArgs
	if terr := decodeArgs(args, &a); terr != nil {
		return nil, terr
	}
	done := task.NormalizeCompletion(task.Completion{
		Summary: a.Summary, Tests: model.TestsStatus(a.TestsStatus), TestsDetail: a.TestsDetail,
	})
	if ps := task.ValidateCompletion(done); ps != nil {
		return nil, invalidArguments(toProblems(ps))
	}
	r, updated, err := t.Store.CompleteTask(ctx, model.Review{
		TaskID: c.TaskID, SessionID: c.SessionID, Summary: done.Summary,
		Tests: done.Tests, TestsDetail: done.TestsDetail, CreatedAt: now,
	})
	if errors.Is(err, store.ErrNoTask) {
		return nil, &ToolError{
			Code:    "no_task",
			Message: "Task " + sanitize.Line(c.TaskID, maxTaskIDEcho) + " does not exist or was dropped.",
		}
	}
	if err != nil {
		return nil, t.internal(completeName, err)
	}
	if !updated && t.Reviewed != nil {
		t.Reviewed(ctx, r, c.Pane)
	}
	return completeResult{Review: r.ID, Task: r.TaskID, Updated: updated, Instruction: instrComplete}, nil
}

// toProblems converts task problems to tool problems.
func toProblems(ps []task.Problem) []Problem {
	out := make([]Problem, len(ps))
	for i, p := range ps {
		out[i] = Problem{Field: p.Field, Problem: p.Problem}
	}
	return out
}
