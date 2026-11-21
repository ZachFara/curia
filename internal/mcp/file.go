package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/task"
)

// file_task's fixed texts and schema (spec F5, Ruling 141).
const (
	fileName        = "file_task"
	fileDescription = "File a task for the user when you notice work outside your own task: a title, a body saying what to do, and the reason you are filing it. The user decides whether it is ever started; you cannot mark it ready or start it, and you may file only a few a day."
	fileSchema      = `{"type":"object","required":["title","reason"],"properties":{"title":{"type":"string","maxLength":120},"body":{"type":"string","maxLength":49152},"reason":{"type":"string","maxLength":300}}}`

	maxFileReason = 300 // characters
	instrFiled    = "Filed. Do not start work on it: only the user can mark it ready or start it. Carry on with your own task."
)

func (t *tools) fileTool() Tool {
	return Tool{Name: fileName, Description: fileDescription, InputSchema: json.RawMessage(fileSchema), Call: t.wrap(fileName, t.file)}
}

type filedArgs struct{ Title, Body, Reason string }

type fileResult struct {
	Task        string           `json:"task"`
	Status      model.TaskStatus `json:"status"`
	Instruction string           `json:"instruction"`
}

// file is file_task: the caller's session files one task in its own
// repository, at the status the setting allows (backlog or ready), under a
// per-session cap the store enforces with the insert. Text is untrusted:
// refused over its limit, then cleaned and stored as data.
func (t *tools) file(ctx context.Context, args json.RawMessage, now time.Time) (any, *ToolError) {
	lvl := t.Cfg.Tasks.AgentFiled
	if lvl.Level == "off" {
		return nil, &ToolError{Code: "filing_off", Message: "Filing tasks is switched off (tasks.agent_filed.level = off). Mention the work in your final summary instead."}
	}
	c := t.Caller(ctx)
	if c.SessionID == "" || c.Repo == "" {
		return nil, &ToolError{Code: "no_session", Message: "This call is not from a Curia-tracked session in a repository, so there is nowhere to file the task."}
	}
	var a filedArgs
	if terr := decodeArgs(args, &a); terr != nil {
		return nil, terr
	}
	// The length checks run on the raw text so a huge value is refused, not silently cut.
	var ps []Problem
	if len(a.Body) > task.MaxBody {
		ps = append(ps, Problem{Field: "body", Problem: fmt.Sprintf("must be at most %d bytes", task.MaxBody)})
	}
	if utf8.RuneCountInString(a.Reason) > maxFileReason {
		ps = append(ps, Problem{Field: "reason", Problem: fmt.Sprintf("must be at most %d characters", maxFileReason)})
	}
	if utf8.RuneCountInString(a.Title) > task.MaxTitle {
		ps = append(ps, Problem{Field: "title", Problem: fmt.Sprintf("must be at most %d characters", task.MaxTitle)})
	}
	reason := sanitize.Line(a.Reason, maxFileReason)
	if reason == "" {
		ps = append(ps, Problem{Field: "reason", Problem: "is required"})
	}
	tk := task.Normalize(model.Task{Title: a.Title, Body: a.Body, Repo: c.Repo, Status: model.TaskBacklog, CreatedAt: now})
	ps = append(ps, toProblems(task.Validate(tk))...)
	if len(ps) > 0 {
		return nil, invalidArguments(ps)
	}
	if lvl.Level == "ready" {
		tk.Status = model.TaskReady
	}
	tk.FiledBy, tk.FiledReason = c.SessionID, reason
	stored, err := t.Store.FileTask(ctx, tk, lvl.MaxPerDay)
	if errors.Is(err, store.ErrFileCap) {
		return nil, &ToolError{Code: "cap_reached", Message: fmt.Sprintf("You have filed %d tasks in the last 24 hours, the most allowed. Put the rest in your final summary.", lvl.MaxPerDay)}
	}
	if err != nil {
		return nil, t.internal(fileName, err)
	}
	return fileResult{Task: stored.ID, Status: stored.Status, Instruction: instrFiled}, nil
}
