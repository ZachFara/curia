package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/task"
)

// taskFiledReasonCells bounds a filed task's reason where it is shown.
const taskFiledReasonCells = 80

// taskFiledNote is " [agent <session>: <reason>]" for a task an agent
// filed, else "". Both are untrusted and cleaned.
func taskFiledNote(t model.Task) string {
	if t.FiledBy == "" {
		return ""
	}
	return fmt.Sprintf(" [agent %s: %s]", sanitize.Line(t.FiledBy, 40), sanitize.Line(t.FiledReason, taskFiledReasonCells))
}

// taskConfirmFiled, for a task an agent filed, prints who filed it, why and
// the whole body (cleaned), then waits for Enter: marking it ready is the
// moment the user reads it (spec F5). End of input cancels.
func taskConfirmFiled(ctx context.Context, cfg config.Config, id string, in io.Reader, out io.Writer) error {
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	t, found, err := st.GetTask(ctx, id)
	_ = st.Close()
	if err != nil || !found || t.FiledBy == "" {
		return err // a missing task is taskUpdate's error; a user's task needs no reading
	}
	_, _ = fmt.Fprintf(out, "%s was filed by agent %s: %s\n\n%s\n\n%s\n\nPress Enter to mark it ready, Ctrl-C to leave it. ",
		id, sanitize.Line(t.FiledBy, 40), sanitize.Line(t.FiledReason, taskFiledReasonCells),
		sanitize.Line(t.Title, task.MaxTitle), sanitize.Block(t.Body, task.MaxBody))
	if _, err = bufio.NewReader(in).ReadString('\n'); err != nil {
		if errors.Is(err, io.EOF) {
			_, _ = fmt.Fprintln(out)
		}
		return fmt.Errorf("%s not marked ready: no Enter", id)
	}
	return nil
}
