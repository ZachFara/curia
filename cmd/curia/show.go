package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/fleet"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/task"
	"github.com/ZachFara/curia/internal/ui"
)

var showJSON bool

func init() {
	c := &cobra.Command{
		Use:   "show <session | T-n | pane>",
		Short: "Show what one agent is doing, without jumping to its pane (the user only)",
		Long: "Prints the dashboard's detail view (`i` there) for one session: transcript tail,\n" +
			"tickets, messages, diff summary, cost and activity, from the same source. The\n" +
			"argument is a session id, a task id (its one session) or a pane id like %12.\n" +
			"The transcript can hold anything the agent saw, so it is for the user only and\n" +
			"is shown cleaned and bounded, also with --json. Read-only.",
		Args:        cobra.ExactArgs(1),
		Annotations: userOnlyMark(),
		RunE:        runShow,
	}
	c.Flags().BoolVar(&showJSON, "json", false, "print the same sections as one JSON object")
	rootCmd.AddCommand(c)
}

// showResolve is the one session arg names: a session id, a task's session or a live pane's.
func showResolve(ctx context.Context, st *store.Store, arg string) (string, error) {
	if s, found, err := st.GetSession(ctx, arg); err != nil || found {
		return s.ID, err
	}
	switch {
	case sendPanePattern.MatchString(arg):
		return showPane(ctx, st, arg)
	case task.ValidID(arg):
		all, err := st.ListSessions(ctx, store.ListFilter{IncludeEnded: true})
		if err != nil {
			return "", err
		}
		var linked, live []string
		for _, s := range all {
			if s.TaskID != arg {
				continue
			}
			linked = append(linked, s.ID)
			if s.State != model.StateEnded && s.State != model.StateStale {
				live = append(live, s.ID)
			}
		}
		if len(live) > 0 {
			linked = live
		}
		switch len(linked) {
		case 0:
			return "", fmt.Errorf("no session for task %s", arg)
		case 1:
			return linked[0], nil
		}
		return "", fmt.Errorf("task %s has more than one session (%s): name one", arg, sanitize.Line(strings.Join(linked, ", "), 200))
	}
	return "", fmt.Errorf("no session %q (a session id, T-<n> or a pane id like %%12)", sanitize.Line(arg, 40))
}

// showPane is the one live session in pane arg of the caller's own tmux
// server (Ruling 133: a pane id is matched only with its server).
func showPane(ctx context.Context, st *store.Store, pane string) (string, error) {
	socket := seenSocket(os.Getenv("TMUX"))
	if socket == "" {
		return "", errors.New("a pane id names a pane on your tmux server, and TMUX is not set here: pass a session id or a task id instead")
	}
	all, err := st.ListSessions(ctx, store.ListFilter{})
	if err != nil {
		return "", err
	}
	var ids []string
	for _, s := range all {
		if s.Pane == pane && !fleet.OtherSocket(socket, s) && s.State != model.StateEnded && s.State != model.StateStale {
			ids = append(ids, s.ID)
		}
	}
	switch len(ids) {
	case 0:
		return "", fmt.Errorf("pane %s is not the pane of a live agent session on this tmux server", pane)
	case 1:
		return ids[0], nil
	}
	return "", fmt.Errorf("pane %s is shared by more than one live session (%s): name one", pane, sanitize.Line(strings.Join(ids, ", "), 200))
}

func runShow(cmd *cobra.Command, args []string) error {
	// The transcript can hold anything another session saw (spec section 7, Ruling 113).
	if err := userOnly("show", "read a session's transcript"); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	id, err := showResolve(ctx, st, args[0])
	if err != nil {
		return err
	}
	src := newDetailSource(st, cfg)
	data, err := src.Detail(ctx, id)
	if err != nil {
		return err
	}
	diff, err := src.Diff(ctx, id)
	if err != nil {
		return err
	}
	if showJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(showJSONOf(data, diff))
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), ui.RenderDetail(data, diff, clock))
	return err
}

type showJSONSession struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	TaskID string `json:"task_id"`
}

// showJSONOf is the detail as JSON; every free text is cleaned and bounded as the view does.
func showJSONOf(d ui.DetailData, diff ui.DetailDiff) map[string]any {
	c := func(s string) string { return sanitize.Line(s, 400) }
	var tickets, messages []map[string]string
	for _, t := range d.Tickets {
		tickets = append(tickets, map[string]string{"id": c(t.ID), "status": c(string(t.Status)), "question": c(t.Question), "answer": c(t.Answer)})
	}
	for _, m := range d.Messages {
		messages = append(messages, map[string]string{"id": c(m.ID), "status": c(ui.MessageState(m)), "text": c(m.Text)})
	}
	s := d.Session
	out := map[string]any{
		"session": showJSONSession{c(s.ID), c(string(s.State)), c(s.Repo), c(s.Branch), c(s.TaskID)},
		"tail":    d.Tail, "tail_note": c(d.TailNote), "tickets": tickets, "messages": messages,
		"cost": map[string]any{"session_usd": s.CostUSD, "session_known": s.CostKnown, "task_usd": d.TaskCostUSD, "task_known": d.TaskCostKnown},
	}
	if d.Budget.Level() != model.BudgetNone {
		out["budget"] = map[string]any{"budget_usd": d.Budget.Limit, "spent_usd": budgetSpent(d.Budget), "level": d.Budget.Level().String()}
	}
	if diff.Has {
		out["diff"] = map[string]string{"stat": sanitize.Block(diff.Stat, 8<<10), "note": c(diff.Note)}
	}
	if d.ActivityOK {
		out["activity"] = d.Activity
	}
	return out
}
