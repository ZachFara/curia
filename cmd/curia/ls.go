package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/fleet"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

// lsJSON is --json.
var lsJSON bool

func init() {
	c := &cobra.Command{
		Use:   "ls",
		Short: "List sessions, most in need of you first",
		Long: "Marks sessions whose tmux pane is gone as stale, then lists every session that\n" +
			"has not ended, ranked by how much it needs you. --json prints a JSON array.",
		Args: cobra.NoArgs,
		RunE: runLs,
	}
	c.Flags().BoolVar(&lsJSON, "json", false, "print a JSON array instead of a table")
	rootCmd.AddCommand(c)
}

func runLs(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	now := clock()
	if err = reconcile(cmd.Context(), st, newTmuxClient(), cfg.LogPath, now); err != nil {
		return err
	}
	sessions, err := st.ListSessions(cmd.Context(), store.ListFilter{HideStaleBefore: now.Add(-cfg.StaleHideAfter)})
	if err != nil {
		return err
	}
	ranked := fleet.Rank(sessions)
	if lsJSON {
		return printJSON(cmd.OutOrStdout(), ranked)
	}
	return printTable(cmd.OutOrStdout(), ranked, now)
}

// lsRow is one session in `curia ls --json`. Its keys are the store's
// column names; a zero time is null. transcript_offset is internal.
type lsRow struct {
	ID              string      `json:"id"`
	Agent           string      `json:"agent"`
	TmuxPane        string      `json:"tmux_pane"`
	TmuxSession     string      `json:"tmux_session"`
	TmuxWindow      string      `json:"tmux_window"`
	Cwd             string      `json:"cwd"`
	Repo            string      `json:"repo"`
	Branch          string      `json:"branch"`
	State           model.State `json:"state"`
	Model           string      `json:"model"`
	ContextUsed     int         `json:"context_used"`
	ContextMax      int         `json:"context_max"`
	SubagentsActive int         `json:"subagents_active"`
	LastUpdate      string      `json:"last_update"`
	SeenAt          *time.Time  `json:"seen_at"`
	TranscriptPath  string      `json:"transcript_path"`
	StartedAt       *time.Time  `json:"started_at"`
	UpdatedAt       *time.Time  `json:"updated_at"`
}

// jsonTime is t in UTC (RFC 3339 once marshaled), or nil for the zero time.
func jsonTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func printJSON(w io.Writer, sessions []model.Session) error {
	rows := make([]lsRow, 0, len(sessions)) // never nil, so no sessions is []
	for _, s := range sessions {
		rows = append(rows, lsRow{
			ID: s.ID, Agent: s.Agent, TmuxPane: s.Pane, TmuxSession: s.TmuxSession, TmuxWindow: s.TmuxWindow,
			Cwd: s.Cwd, Repo: s.Repo, Branch: s.Branch, State: s.State, Model: s.Model,
			ContextUsed: s.ContextUsed, ContextMax: s.ContextMax, SubagentsActive: s.SubagentsActive,
			LastUpdate: s.LastUpdate, SeenAt: jsonTime(s.SeenAt), TranscriptPath: s.TranscriptPath,
			StartedAt: jsonTime(s.StartedAt), UpdatedAt: jsonTime(s.UpdatedAt),
		})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}

// printTable prints one row per session. Every cell is session text, which
// is untrusted, so it goes through cell before reaching the terminal.
func printTable(w io.Writer, sessions []model.Session, now time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "STATE\tPANE\tREPO\tBRANCH\tAGE\tLAST UPDATE")
	for _, s := range sessions {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", cell(string(s.State)), cell(s.Pane),
			cell(s.Repo), cell(s.Branch), age(s.UpdatedAt, now), cell(s.LastUpdate))
	}
	return tw.Flush()
}

// cell makes s safe to print in a table: every character that is not
// printable (control characters such as ESC, C1 codes, tabs, line breaks,
// and invisible format characters such as bidi overrides) becomes '?', so
// no escape sequence reaches the terminal and the row stays one line.
// An empty cell is "-".
func cell(s string) string {
	if s == "" {
		return "-"
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) { // includes the ASCII space
			return r
		}
		return '?'
	}, s)
}

// age is how long before now t was, in its largest whole unit; "-" for the
// zero time.
func age(t, now time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := max(now.Sub(t), 0)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}
