package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/decision"
	"github.com/ZachFara/curia/internal/fleet"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
	"github.com/ZachFara/curia/internal/ui"
)

// Flags of `curia decisions`; tests reset them.
var (
	decisionsStatus       string
	decisionsJSON         bool
	decisionsScope        string
	decisionsSupersede    bool
	decisionsSupersededBy string
)

// Cell limits of the decisions table.
const (
	decisionsQuestionLimit = 70
	decisionsAnswerLimit   = 40
)

func init() {
	parent := &cobra.Command{
		Use:   "decisions",
		Short: "List and edit decision tickets",
	}
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List decision tickets",
		Long: "Expires assume tickets that are due, then lists tickets by --status: open\n" +
			"(the default; with it every answer the agent has not got yet), decided\n" +
			"(answered, confirmed, overridden), closed (withdrawn, expired) or all, in\n" +
			"allocation (id) order. --json prints a JSON array.",
		Args: cobra.NoArgs,
		RunE: runDecisionsLs,
	}
	ls.Flags().StringVar(&decisionsStatus, "status", "open", "open (and answers not yet delivered), decided, closed or all")
	ls.Flags().BoolVar(&decisionsJSON, "json", false, "print a JSON array instead of a table")
	edit := &cobra.Command{
		Use:         "edit <id>",
		Short:       "Rescope or supersede a decided ticket (the user only)",
		Annotations: userOnlyMark(),
		Args:        cobra.ExactArgs(1),
		RunE:        runDecisionsEdit,
	}
	edit.Flags().StringVar(&decisionsScope, "scope", "", "new scope: task, repo or global")
	edit.Flags().BoolVar(&decisionsSupersede, "supersede", false, "mark the ticket superseded")
	edit.Flags().StringVar(&decisionsSupersededBy, "superseded-by", "", "id of the decided ticket that replaces it (implies --supersede)")
	parent.AddCommand(ls, edit)
	rootCmd.AddCommand(parent)
}

// userOnlyAnnotation marks a command that only the user may run; such a
// command calls userOnly before anything else, which a test checks for
// every command so marked.
const userOnlyAnnotation = "curia-user-only"

// userOnlyMark is the Annotations value that marks a command user-only.
func userOnlyMark() map[string]string { return map[string]string{userOnlyAnnotation: "yes"} }

// userOnly is the one guard of the commands only the user may run (Rulings
// 112 and 119): it refuses when the caller looks like an agent (agentSign),
// has no controlling terminal, or descends from an agent (agentAncestor),
// saying which, as "curia <command> cannot run …: only the user may
// <what>". An agent's tool shell has no controlling terminal, so clearing
// its variables is not enough, and a pseudo-terminal of its own (script,
// expect) does not change whose child it is; a script or cron job cannot
// act as the user either.
func userOnly(command, what string) error {
	if sign := agentSign(); sign != "" {
		return fmt.Errorf("curia %s cannot run inside an agent session (%s): only the user may %s", command, sign, what)
	}
	if err := userTerminalOpen(); err != nil {
		return fmt.Errorf("curia %s cannot run without a terminal (no controlling terminal: %s): only the user may %s, at a terminal",
			command, sanitize.Line(err.Error(), 120), what)
	}
	if why := agentAncestor(); why != "" {
		return fmt.Errorf("curia %s cannot run inside an agent session (%s): only the user may %s", command, why, what)
	}
	return nil
}

// userAncestryLimit bounds the walk up the parent processes.
const userAncestryLimit = 64

// userProc is what the ancestry walk reads of one process: its parent pid,
// its command name (ps's comm: the executable, as a path or a base name)
// and, for an interpreter, its arguments.
type userProc struct {
	PPID int
	Comm string
	Args string // read only when Comm is an interpreter (claudeInterpreters)
}

// userProcesses reads the process table once (re-review A, M8): every
// process's parent, command name and, for an interpreter, arguments. Tests
// replace it with a table of their own; the test default fails a test that
// has not set one.
var userProcesses = psTable

// psPaths are where ps is looked for, by absolute path only, never on PATH
// (re-review A, M10).
var psPaths = []string{"/bin/ps", "/usr/bin/ps"}

// psPath is the first of psPaths that exists.
func psPath() (string, error) {
	for _, p := range psPaths {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("no ps at %s", strings.Join(psPaths, " or "))
}

// psTable is userProcesses from ps under one deadline: `ps -A -o
// pid=,ppid=,comm=`, then `ps -A -o pid=,args=` when an interpreter
// (claudeInterpreters) is running. One snapshot, so the walk sees one
// consistent table.
func psTable() (map[int]userProc, error) {
	ps, err := psPath()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*subprocessTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, ps, "-A", "-o", "pid=,ppid=,comm=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps -A: %w", err)
	}
	table := map[int]userProc{}
	interpreter := false
	for line := range strings.Lines(string(out)) {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, perr := strconv.Atoi(f[0])
		ppid, qerr := strconv.Atoi(f[1])
		if perr != nil || qerr != nil {
			return nil, fmt.Errorf("ps -A printed %q", sanitize.Line(line, 80))
		}
		table[pid] = userProc{PPID: ppid, Comm: procComm(line)}
		interpreter = interpreter || slices.Contains(claudeInterpreters, filepath.Base(table[pid].Comm))
	}
	if !interpreter {
		return table, nil
	}
	if err = psArgs(ctx, ps, table); err != nil {
		return nil, err
	}
	return table, nil
}

// psArgs fills in the arguments of every interpreter in table from `ps -A
// -o pid=,args=`.
func psArgs(ctx context.Context, ps string, table map[int]userProc) error {
	out, err := exec.CommandContext(ctx, ps, "-A", "-o", "pid=,args=").Output()
	if err != nil {
		return fmt.Errorf("ps -A: %w", err)
	}
	for line := range strings.Lines(string(out)) {
		id, args, _ := strings.Cut(strings.TrimSpace(line), " ")
		pid, err := strconv.Atoi(id)
		if p, ok := table[pid]; err == nil && ok && slices.Contains(claudeInterpreters, filepath.Base(p.Comm)) {
			p.Args = strings.TrimSpace(args)
			table[pid] = p
		}
	}
	return nil
}

// procComm is the comm column of one `pid ppid comm` line: everything after
// the second field, which may hold spaces.
func procComm(line string) string {
	rest := strings.TrimSpace(line)
	for range 2 {
		_, rest, _ = strings.Cut(rest, " ")
		rest = strings.TrimLeft(rest, " ")
	}
	return strings.TrimRight(rest, " \n")
}

// claudeInterpreters are the programs that run Claude Code's JavaScript
// CLI when it is installed from npm.
var claudeInterpreters = []string{"node", "bun"}

// isClaudeCode reports whether p is Claude Code. Seen on the dev Mac (Claude
// Code 2.1.325, native install): comm "claude" or ".../.local/bin/claude",
// args "claude --continue ...". An npm install runs as node (or bun) with
// the script as its first argument: ".../bin/claude" through npm's link, or
// ".../@anthropic-ai/claude-code/cli.js". The match is on those base names
// only, never on any argument that merely holds the word.
func isClaudeCode(p userProc) bool {
	if filepath.Base(p.Comm) == "claude" {
		return true
	}
	if !slices.Contains(claudeInterpreters, filepath.Base(p.Comm)) {
		return false
	}
	for _, a := range strings.Fields(p.Args)[min(1, len(strings.Fields(p.Args))):] {
		if strings.HasPrefix(a, "-") {
			continue // the interpreter's own flags
		}
		dir, base := filepath.Base(filepath.Dir(a)), filepath.Base(a)
		return base == "claude" || (dir == "claude-code" && (base == "cli.js" || base == "cli.mjs"))
	}
	return false
}

// agentAncestor says why the caller descends from an agent, or "" when it
// does not (Ruling 119): walking from the parent to pid 1, a process is
// Claude Code (isClaudeCode) or the first process of the pane of a live
// tracked session (agentPanePIDs). It fails closed: an ancestry it cannot
// read, one deeper than userAncestryLimit, or a store it cannot read are an
// agent's; with no readable pane list it relies on the Claude Code ancestor
// alone (Ruling 123). Curia records no process id for a session, so only
// these are checked.
func agentAncestor() string {
	table, err := userProcesses()
	if err != nil {
		return "its parent processes cannot be read: " + sanitize.Line(err.Error(), 120)
	}
	panes, why := agentPanePIDs()
	if why != "" {
		return why
	}
	pid := os.Getppid()
	for range userAncestryLimit {
		if pid <= 1 {
			// Reaching pid 1 with no agent on the way is the user's: a
			// terminal, a tmux popup or run-shell job (through the tmux
			// server), or an orphan. Requiring a known terminal program
			// would refuse users of any other one and still let a run-shell
			// job through, which has the ancestry of the user's own popup:
			// a process of the same operating-system user can always detach
			// itself this way (the stated limit of these guards).
			return ""
		}
		p, ok := table[pid]
		if !ok {
			return fmt.Sprintf("its parent processes cannot be read: process %d is not in the process table", pid)
		}
		switch {
		case isClaudeCode(p):
			return fmt.Sprintf("it runs under Claude Code (process %d)", pid)
		case panes[pid] != "":
			return fmt.Sprintf("it runs under the pane of agent session %s (process %d)", spawnShortID(panes[pid]), pid)
		}
		pid = p.PPID
	}
	return fmt.Sprintf("its parent processes are more than %d deep", userAncestryLimit)
}

// ancestorSession is the live tracked session whose pane's first process
// is the nearest ancestor of this process to be one (the agent running in
// that pane started it), "" when the walk reaches pid 1 without one; ok is
// false when the ancestry or the panes cannot be read, or the walk is
// deeper than userAncestryLimit (Ruling 113: who sent a message is read
// from the processes, not from variables the sender sets).
func ancestorSession() (id string, ok bool) {
	table, err := userProcesses()
	if err != nil {
		return "", false
	}
	panes, why := agentPanePIDs()
	if why != "" {
		return "", false
	}
	pid := os.Getppid()
	for range userAncestryLimit {
		if pid <= 1 {
			return "", true
		}
		if id := panes[pid]; id != "" {
			return id, true
		}
		p, found := table[pid]
		if !found {
			return "", false
		}
		pid = p.PPID
	}
	return "", false
}

// agentPanePIDs maps the pid of the first process of each pane holding a
// live tracked session (neither ended nor stale) to that session's id, or
// says why it cannot when the store cannot be read, and then the caller is
// refused. When the tmux pane list cannot be read, or the server the
// caller's TMUX reaches shows none of the live agents' panes, the pane
// check is skipped (Ruling 123): the map is empty, one INFO line says why,
// and the guard decides on the Claude Code ancestor, the terminal and the
// environment alone, so a tmux server that is down or restarted does not
// lock the user out. With no store yet, or no live session in a pane, the
// map is empty.
func agentPanePIDs() (map[int]string, string) {
	out := map[int]string{}
	cfg, err := loadConfig()
	if err != nil {
		return nil, "the config cannot be read to check for agents: " + sanitize.Line(err.Error(), 120)
	}
	if _, err = os.Stat(cfg.DBPath); errors.Is(err, fs.ErrNotExist) {
		return out, ""
	}
	st, err := openStore(cfg)
	if err != nil {
		return nil, "the store cannot be read to check for agents"
	}
	defer func() { _ = st.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*subprocessTimeout)
	defer cancel()
	sessions, err := st.ListSessions(ctx, store.ListFilter{})
	if err != nil {
		return nil, "the store cannot be read to check for agents"
	}
	sessions = slices.DeleteFunc(sessions, func(s model.Session) bool {
		return s.Pane == "" || s.State == model.StateEnded || s.State == model.StateStale
	})
	if len(sessions) == 0 {
		return out, ""
	}
	panes, err := newTmuxClient().ListPanes(ctx)
	if err != nil {
		logInfo(cfg.LogPath, "user check: agent pane check skipped: tmux cannot list its panes", "err", sanitize.Line(err.Error(), 200))
		return out, ""
	}
	// A session counts only on a server consistent with it (decisionsOnServer,
	// the rules reconcile uses): not recorded on another server, its pane
	// listed and the server no newer than the session. Another server
	// reached through the caller's TMUX, whose pane ids may collide, is then
	// the Ruling 123 case.
	shown := false
	for _, s := range sessions {
		if !decisionsOnServer(panes, s) {
			continue
		}
		shown = true
		for _, p := range panes {
			if p.ID == s.Pane && p.PID > 1 {
				out[p.PID] = s.ID
			}
		}
	}
	if !shown {
		logInfo(cfg.LogPath, "user check: agent pane check skipped: the tmux server reached shows none of the live agents' panes")
	}
	return out, ""
}

// userOnlyRunE is run behind userOnly, as a cobra RunE.
func userOnlyRunE(command, what string, run func(*cobra.Command, []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		if err := userOnly(command, what); err != nil {
			return err
		}
		return run(cmd, args)
	}
}

// agentSign says why the caller looks like an agent, or "" when nothing
// does (Ruling 112): Claude Code sets CLAUDECODE and CLAUDE_CODE_SESSION_ID
// in every command it runs, and an agent's shell has TMUX_PANE set to the
// pane of its live session. A pane match is checked against tmux first
// (reconcile's rules), so a session whose pane is gone, whose pane id a tmux
// restart gave to someone else, or that is recorded on another tmux server
// than the one reached (pane ids are reused across servers), does not
// count. When the store cannot be read the answer is an agent: Curia cannot
// tell.
func agentSign() string {
	for _, name := range []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID"} {
		if os.Getenv(name) != "" {
			// An agent's tmux command can leave it in the tmux server's global
			// environment, and every new shell there inherits it (re-review A,
			// M6).
			return name + " is set; if you are not an agent, tmux may hold it: run tmux set-environment -gu " + name +
				" and open a new shell"
		}
	}
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		return ""
	}
	cfg, err := loadConfig()
	if err != nil {
		return "TMUX_PANE is set and the config cannot be read to check it"
	}
	if _, err = os.Stat(cfg.DBPath); errors.Is(err, fs.ErrNotExist) {
		return "" // no store yet, so no session is tracked anywhere
	}
	st, err := openStore(cfg)
	if err != nil {
		return "TMUX_PANE is set and the store cannot be read to check it"
	}
	defer func() { _ = st.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*subprocessTimeout)
	defer cancel()
	// Read only: the guard never reconciles or writes (re-review A, I2). A
	// session in this pane counts only when the server reached is consistent
	// with it (decisionsOnServer); when tmux cannot answer the ancestry decides.
	// Each server can have a live session in a pane with this id, so every
	// one is checked.
	sessions, err := agentInPane(ctx, st, pane)
	if err != nil {
		return "TMUX_PANE is set and the store cannot be read to check it"
	}
	if len(sessions) == 0 {
		return ""
	}
	panes, err := newTmuxClient().ListPanes(ctx)
	if err != nil {
		return ""
	}
	for _, s := range sessions {
		if decisionsOnServer(panes, s) {
			return "this is pane " + sanitize.Line(pane, 20) + " of agent session " + spawnShortID(s.ID) +
				"; if no agent runs here any more, run curia ls, which marks a session whose pane is gone, or end it"
		}
	}
	return ""
}

// agentInPane finds the sessions in pane that are neither ended nor stale.
func agentInPane(ctx context.Context, st *store.Store, pane string) ([]model.Session, error) {
	sessions, err := st.ListSessions(ctx, store.ListFilter{})
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(sessions, func(s model.Session) bool {
		return s.Pane != pane || s.State == model.StateEnded || s.State == model.StateStale
	}), nil
}

// decisionsOnServer reports whether live session s is in a pane of the tmux
// server whose panes are live, by reconcile's rules and without writing: s
// is not recorded on another server (fleet.OtherServer), and its pane is
// listed and not from before a restart (fleet.Gone).
func decisionsOnServer(live []tmux.Pane, s model.Session) bool {
	return !fleet.OtherServer(live, s) && !fleet.Gone(live, s)
}

// userTerminalOpen opens and closes the controlling terminal; tests
// replace it (the test default fails a test that has not set its own).
var userTerminalOpen = func() error {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return tty.Close()
}

// userAtTerminal reports whether the caller is the user at a terminal
// (Rulings 112 and 119): userOnly would let it run. A tmux popup or
// run-shell job descends from the tmux server, not from an agent.
func userAtTerminal() bool {
	return userOnly("", "") == nil
}

// decisionsStatuses maps a --status value to the statuses it lists; nil
// means all.
func decisionsStatuses(name string) ([]model.DecisionStatus, error) {
	switch name {
	case "open":
		return []model.DecisionStatus{model.StatusOpen}, nil
	case "decided":
		return []model.DecisionStatus{model.StatusAnswered, model.StatusConfirmed, model.StatusOverridden}, nil
	case "closed":
		return []model.DecisionStatus{model.StatusWithdrawn, model.StatusExpired}, nil
	case "all":
		return nil, nil
	}
	return nil, fmt.Errorf("invalid --status %q: want open, decided, closed or all", sanitize.Line(name, 40))
}

func runDecisionsLs(cmd *cobra.Command, _ []string) error {
	statuses, err := decisionsStatuses(decisionsStatus)
	if err != nil {
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
	now := clock()
	if _, err = st.ExpireDecisions(cmd.Context(), now); err != nil {
		return err
	}
	// The default listing also shows the answers the agent has not got yet:
	// an agent may be waiting on one with nothing else showing it (M3).
	ds, err := st.ListDecisions(cmd.Context(), store.DecisionFilter{Statuses: statuses, Undelivered: decisionsStatus == "open"})
	if err != nil {
		return err
	}
	if decisionsJSON {
		return decisionsPrintJSON(cmd.OutOrStdout(), ds)
	}
	sessions, err := st.ListSessions(cmd.Context(), store.ListFilter{IncludeEnded: true})
	if err != nil {
		return err
	}
	states := make(map[string]model.State, len(sessions))
	for _, s := range sessions {
		states[s.ID] = s.State
	}
	return decisionsPrintTable(cmd.OutOrStdout(), ds, states, now)
}

// decisionsRow is one ticket in `curia decisions ls --json`. Its keys are
// the columns of the decisions table except n; a zero time is null.
type decisionsRow struct {
	ID             string               `json:"id"`
	SessionID      string               `json:"session_id"`
	TaskID         string               `json:"task_id"`
	Repo           string               `json:"repo"`
	Question       string               `json:"question"`
	Context        string               `json:"context"`
	Options        []model.Option       `json:"options"`
	Recommendation string               `json:"recommendation"`
	Rationale      string               `json:"rationale"`
	Reversibility  model.Reversibility  `json:"reversibility"`
	Mode           model.Mode           `json:"mode"`
	Assumption     string               `json:"assumption"`
	Scope          model.Scope          `json:"scope"`
	Tags           []string             `json:"tags"`
	Status         model.DecisionStatus `json:"status"`
	AnswerKey      string               `json:"answer_key"`
	Answer         string               `json:"answer"`
	AnswerReason   string               `json:"answer_reasoning"`
	AnsweredVia    model.AnsweredVia    `json:"answered_via"`
	PrecedentOf    string               `json:"precedent_of"`
	DiffersFrom    string               `json:"differs_from"`
	DiffersWhy     string               `json:"differs_why"`
	WithdrawReason string               `json:"withdraw_reason"`
	SupersededBy   string               `json:"superseded_by"`
	ArgsHash       string               `json:"args_hash"`
	Delivery       model.Delivery       `json:"delivery"`
	DeliveredVia   model.DeliveryVia    `json:"delivered_via"`
	CreatedAt      *time.Time           `json:"created_at"`
	ExpiresAt      *time.Time           `json:"expires_at"`
	AnsweredAt     *time.Time           `json:"answered_at"`
	ClosedAt       *time.Time           `json:"closed_at"`
	SupersededAt   *time.Time           `json:"superseded_at"`
	DeliveredAt    *time.Time           `json:"delivered_at"`
	ConfirmedAt    *time.Time           `json:"confirmed_at"`
}

func decisionsToRow(d model.Decision) decisionsRow {
	opts, tags := d.Options, d.Tags
	if opts == nil {
		opts = []model.Option{}
	}
	if tags == nil {
		tags = []string{}
	}
	return decisionsRow{
		ID: d.ID, SessionID: d.SessionID, TaskID: d.TaskID, Repo: d.Repo, Question: d.Question,
		Context: d.Context, Options: opts, Recommendation: d.Recommendation, Rationale: d.Rationale,
		Reversibility: d.Reversibility, Mode: d.Mode, Assumption: d.Assumption, Scope: d.Scope, Tags: tags,
		Status: d.Status, AnswerKey: d.AnswerKey, Answer: d.Answer, AnswerReason: d.AnswerReasoning,
		AnsweredVia: d.AnsweredVia, PrecedentOf: d.PrecedentOf, DiffersFrom: d.DiffersFrom,
		DiffersWhy: d.DiffersWhy, WithdrawReason: d.WithdrawReason, SupersededBy: d.SupersededBy,
		ArgsHash: d.ArgsHash, Delivery: d.Delivery, DeliveredVia: d.DeliveredVia,
		CreatedAt: jsonTime(d.CreatedAt), ExpiresAt: jsonTime(d.ExpiresAt), AnsweredAt: jsonTime(d.AnsweredAt),
		ClosedAt: jsonTime(d.ClosedAt), SupersededAt: jsonTime(d.SupersededAt), DeliveredAt: jsonTime(d.DeliveredAt),
		ConfirmedAt: jsonTime(d.ConfirmedAt),
	}
}

func decisionsPrintJSON(w io.Writer, ds []model.Decision) error {
	rows := make([]decisionsRow, 0, len(ds)) // never nil, so no tickets is []
	for _, d := range ds {
		rows = append(rows, decisionsToRow(d))
	}
	return writeJSON(w, rows, "  ")
}

// decisionsCell makes ticket text, which is untrusted, safe as one table cell.
func decisionsCell(s string, limit int) string { return ui.Cell(sanitize.Line(s, limit)) }

// decisionsPrintTable prints one row per ticket, in allocation (id) order,
// with where it was answered (VIA) and how far its delivery got (M3);
// states holds the state of each known session.
func decisionsPrintTable(w io.Writer, ds []model.Decision, states map[string]model.State, now time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tSTATUS\tMODE\tAGE\tREPO\tQUESTION\tANSWER\tVIA\tDELIVERY")
	for _, d := range ds {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			decisionsCell(d.ID, decisionsQuestionLimit), decisionsCell(decisionsStatusText(d), decisionsQuestionLimit),
			decisionsCell(string(d.Mode), decisionsQuestionLimit), decisionsCell(ui.Age(d.CreatedAt, now), decisionsQuestionLimit), decisionsCell(ui.RepoName(d.Repo), decisionsQuestionLimit),
			decisionsCell(d.Question, decisionsQuestionLimit),
			decisionsCell(d.Answer, decisionsAnswerLimit), decisionsCell(decisionsOr(string(d.AnsweredVia), "-"), decisionsQuestionLimit),
			decisionsCell(decisionsDeliveryText(d, states), decisionsQuestionLimit))
	}
	return tw.Flush()
}

// decisionsOr is s, or alt when s is empty.
func decisionsOr(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

// decisionsStatusText is d's status, marked when the user superseded it.
func decisionsStatusText(d model.Decision) string {
	if !d.SupersededAt.IsZero() {
		return string(d.Status) + " (superseded)"
	}
	return string(d.Status)
}

// decisionsDeliveryText is how far d's delivery got: "delivered
// (stop_hook)", "typed, unconfirmed" for a typed answer no submitted prompt
// or check_decision has confirmed yet (Ruling 86), "queued" or "none". An
// answer not delivered yet whose session (in states) has ended, gone stale
// or has no row is marked so: it is not on the badge (live check 3).
func decisionsDeliveryText(d model.Decision, states map[string]model.State) string {
	text := string(d.Delivery)
	switch {
	case d.Delivery == model.DeliveryDelivered && d.DeliveredVia == model.ViaSendKeys && d.ConfirmedAt.IsZero():
		text = "typed, unconfirmed"
	case d.Delivery == model.DeliveryDelivered && d.DeliveredVia != "":
		return text + " (" + string(d.DeliveredVia) + ")"
	case d.Delivery != model.DeliveryQueued:
		return text
	}
	switch state, known := states[d.SessionID]; {
	case !known:
		return text + " (no session row)"
	case state == model.StateEnded || state == model.StateStale:
		return text + " (session " + string(state) + ")"
	}
	return text
}

func runDecisionsEdit(cmd *cobra.Command, args []string) error {
	if err := userOnly("decisions edit", "curate a decision"); err != nil {
		return err
	}
	id := args[0]
	if !decision.ValidID(id) {
		return fmt.Errorf("invalid decision id %q: want D-<number>", sanitize.Line(id, 40))
	}
	e, err := decisionsEditFromFlags()
	if err != nil {
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
	// The previous scope and supersession are only for the messages;
	// EditDecision decides.
	before, _, err := st.GetDecision(cmd.Context(), id)
	if err != nil {
		return err
	}
	after, err := st.EditDecision(cmd.Context(), id, e, clock())
	if err != nil {
		return fmt.Errorf("%s: %w", id, err)
	}
	decisionsPrintEdit(cmd.OutOrStdout(), id, e, before, after)
	return nil
}

// decisionsPrintEdit says what edit e changed in ticket id, from before to
// after, and where a supersession's retraction goes.
func decisionsPrintEdit(out io.Writer, id string, e model.DecisionEdit, before, after model.Decision) {
	if e.Scope != "" {
		_, _ = fmt.Fprintf(out, "%s scope %s -> %s\n", id, before.Scope, after.Scope)
	}
	switch {
	case e.SupersededBy != "":
		_, _ = fmt.Fprintf(out, "%s superseded by %s\n", id, after.SupersededBy)
	case e.Supersede:
		_, _ = fmt.Fprintf(out, "%s superseded\n", id)
	}
	switch {
	case !e.Supersede || after.SessionID == "":
	case after.Delivery == model.DeliveryQueued:
		// The retraction goes by the Stop hook or check_decision (Ruling 82).
		_, _ = fmt.Fprintf(out, "%s retraction queued for the agent's Stop hook, or tell the agent to call check_decision %s\n", id, id)
	case before.SupersededAt.IsZero() && after.Delivery == model.DeliveryDelivered:
		// N3: the asking session has ended or gone; nobody could collect it.
		_, _ = fmt.Fprintf(out, "%s: its session has ended; nothing to retract\n", id)
	}
}

// decisionsEditFromFlags builds the edit the flags ask for.
func decisionsEditFromFlags() (model.DecisionEdit, error) {
	e := model.DecisionEdit{
		Scope:        model.Scope(decisionsScope),
		Supersede:    decisionsSupersede || decisionsSupersededBy != "",
		SupersededBy: decisionsSupersededBy,
	}
	switch e.Scope {
	case "", model.ScopeTask, model.ScopeRepo, model.ScopeGlobal:
	default:
		return e, fmt.Errorf("invalid --scope %q: want task, repo or global", sanitize.Line(decisionsScope, 40))
	}
	if e.SupersededBy != "" && !decision.ValidID(e.SupersededBy) {
		return e, fmt.Errorf("invalid --superseded-by %q: want D-<number>", sanitize.Line(e.SupersededBy, 40))
	}
	if e.Scope == "" && !e.Supersede {
		return e, errors.New("nothing to change: give --scope, --supersede or --superseded-by")
	}
	return e, nil
}
