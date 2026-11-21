package main

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/attention"
	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/deliver"
	"github.com/ZachFara/curia/internal/fleet"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/task"
	"github.com/ZachFara/curia/internal/tmux"
	"github.com/ZachFara/curia/internal/ui"
)

// restoreAction is "restore", or a skip reason of section 4.10's table, as
// printed in the plan.
type restoreAction string

// The actions of a plan row. live and exited rows are left out of the plan.
const (
	restoreDo           restoreAction = "restore"
	restoreLive         restoreAction = "live"
	restoreExited       restoreAction = "exited"
	restoreEnded        restoreAction = "ended"
	restoreOtherServer  restoreAction = "other server"
	restoreOutside      restoreAction = "outside tmux"
	restoreClosedHere   restoreAction = "closed here"
	restoreTeammate     restoreAction = "teammate"
	restoreNotClaude    restoreAction = "not Claude Code"
	restoreOtherAgent   restoreAction = "other agent"
	restoreBadID        restoreAction = "bad id"
	restorePaused       restoreAction = "paused"
	restoreNoDir        restoreAction = "no directory"
	restoreNoTranscript restoreAction = "no transcript"
	restoreTooOld       restoreAction = "too old"
	restoreSuperseded   restoreAction = "superseded"
	restoreOverLimit    restoreAction = "over the limit"
)

// restoreRow is one line of the plan.
type restoreRow struct {
	Session    model.Session
	Task       model.Task // zero when the session has no usable task
	Linked     bool       // the task exists and is not dropped
	LastActive time.Time  // the transcript's modification time; zero when there is none
	Action     restoreAction
	Detail     string // what follows "skip:" for the user, already clean
}

var (
	restoreLookPath = exec.LookPath // finds claude; tests replace it
	restoreBin      = executablePath
	restoreSleep    = time.Sleep // tests replace it and clock
	// restoreIsTerminal reports whether stdin and stdout are terminals; tests replace it.
	restoreIsTerminal = func() bool { return taskIsTerminal(os.Stdin) && taskIsTerminal(os.Stdout) }
)

const (
	restoreWait = 30 * time.Second
	restorePoll = 250 * time.Millisecond
	restoreMax  = 32
	restoreHold = 2 * time.Minute
	// restoreExecHold is how long the claim is held from the confirmation:
	// the windows, the wait for them and the notices (I6).
	restoreExecHold = 10 * time.Minute
	restoreLeadText = "Curia restored this session after a restart. Your teammates did not survive it: respawn your team from the shared task list, then continue."
)

const (
	restoreListLimit = 2 * time.Second // the first ListPanes
	restoreScan      = 1000            // the most candidates read from the store
	// restoreEndReason is the end reason of a row restore sets ended (a
	// teammate of a restored lead, an older session of a restored task, the
	// old row of a resume that got a new id), so it is never offered again.
	restoreEndReason = "curia_restore"
	restorePathMax   = 4096
)

// restoreSessionID is the form of a Claude Code session id.
var restoreSessionID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// restoreNameRun is a run of runes a tmux name may not hold.
var restoreNameRun = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// The flags of curia restore.
var (
	restoreDryRun        bool
	restoreYes           bool
	restoreIncludeClosed bool
	restoreSince         time.Duration
)

func init() {
	c := &cobra.Command{
		Use:   "restore",
		Short: "Reopen the agent sessions a reboot or tmux crash closed (the user only)",
		Long: "Lists the Claude Code sessions the store holds that no tmux pane runs any more\n" +
			"(killed, or ended when tmux went down) and that were active within --since\n" +
			"(restore.max_age, 24h, by default), shows the plan, and on confirmation opens\n" +
			"one tmux window per session with claude --resume (curia launch for a task's\n" +
			"session). A session whose pane was closed while the tmux server kept running\n" +
			"(it started after the server did: you closed it, or killed it from the\n" +
			"dashboard) is skipped unless --include-closed is given. It never touches a\n" +
			"window that exists and types nothing itself; a restored team lead gets a\n" +
			"message asking it to respawn its team. Refuses to run inside an agent session.",
		Annotations: userOnlyMark(),
		Args:        cobra.NoArgs,
		RunE:        runRestore,
	}
	c.Flags().BoolVar(&restoreDryRun, "dry-run", false, "print the plan and change nothing")
	c.Flags().BoolVar(&restoreYes, "yes", false, "restore without asking")
	c.Flags().BoolVar(&restoreIncludeClosed, "include-closed", false, "also restore sessions whose pane was closed while tmux kept running")
	c.Flags().DurationVar(&restoreSince, "since", 0, "how far back to look (default: restore.max_age)")
	rootCmd.AddCommand(c)
}

// restoreEnv is what one run of curia restore works with.
type restoreEnv struct {
	st  *store.Store
	tm  tmux.Client
	log *slog.Logger
	in  io.Reader
	out io.Writer
}

func runRestore(cmd *cobra.Command, _ []string) error {
	// No agent may start agents (spec section 7).
	if err := userOnly("restore", "restore sessions"); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	since := cmp.Or(restoreSince, cfg.Restore.MaxAge)
	if since < 0 {
		return fmt.Errorf("--since %v: must be above zero", since)
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	logger, closeLog, err := openLog(cfg.LogPath)
	if err != nil {
		return err
	}
	defer func() { _ = closeLog() }()
	e := restoreEnv{st: st, tm: newTmuxClient(), log: logger, in: cmd.InOrStdin(), out: cmd.OutOrStdout()}
	return restoreRun(cmd.Context(), e, cfg, since)
}

// restoreRun is section 4.10, steps 1 to 8.
func restoreRun(ctx context.Context, e restoreEnv, cfg config.Config, since time.Duration) error {
	panes, err := restoreListPanes(ctx, e.tm, restoreListLimit)
	if err != nil {
		return errors.New("no tmux server is running: start tmux, then run curia restore again")
	}
	claimed := clock()
	ok, err := e.st.ClaimRestore(ctx, claimed, restoreHold)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("another curia restore is running")
	}
	defer func() {
		if rerr := e.st.ReleaseRestore(context.WithoutCancel(ctx)); rerr != nil {
			e.log.Warn("restore: release the claim", "err", rerr.Error())
		}
	}()
	rows, err := restoreLoad(ctx, e, cfg, panes, claimed, since)
	if err != nil {
		return err
	}
	restorePrintPlan(e.out, rows, claimed)
	restorePrintByHand(e.out, rows)
	todo := restoreTodo(rows)
	if len(todo) == 0 {
		_, err = fmt.Fprintf(e.out, "nothing to restore from the last %v (curia restore --since <duration> looks further back)\n", since)
		return err
	}
	if restoreDryRun {
		return nil
	}
	if yes, err := restoreConfirm(e, len(todo)); err != nil || !yes {
		return err
	}
	if clock().Sub(claimed) > restoreHold {
		return fmt.Errorf("the answer took longer than %v, so another restore may have started: run curia restore again", restoreHold)
	}
	// Hold the claim for the windows, the wait and the notices, not just the
	// question: a claim older than restoreHold is taken over, so it is dated
	// that much before the end of restoreExecHold.
	if ok, err := e.st.RenewRestore(ctx, clock().Add(restoreExecHold-restoreHold)); err != nil || !ok {
		return cmp.Or(err, errors.New("another curia restore took the claim over: run curia restore again"))
	}
	return restoreExecute(ctx, e, rows, todo)
}

// restoreListPanes lists the server's panes within limit.
func restoreListPanes(ctx context.Context, tm tmux.Client, limit time.Duration) ([]tmux.Pane, error) {
	tctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	return tm.ListPanes(tctx)
}

// restoreLoad reconciles (not on a dry run, which changes nothing) and
// classifies the candidates: store.RestoreCandidates (Ruling 117), plus the
// ended sessions with no recorded end reason, shown as skipped.
func restoreLoad(ctx context.Context, e restoreEnv, cfg config.Config, panes []tmux.Pane, now time.Time, since time.Duration) ([]restoreRow, error) {
	if !restoreDryRun {
		windows, err := fleet.Reconcile(ctx, e.st, panes, now)
		applyMarkers(ctx, e.tm, e.st, cfg.LogPath, "restore attention marker", fleet.Socket(panes), windows)
		if err != nil {
			return nil, err
		}
	}
	sessions, err := e.st.RestoreCandidates(ctx, now.Add(-since), restoreScan)
	if err != nil {
		return nil, err
	}
	all, err := e.st.ListSessions(ctx, store.ListFilter{IncludeEnded: true})
	if err != nil {
		return nil, err
	}
	for _, s := range all {
		if s.State == model.StateEnded && s.EndReason == "" && !s.UpdatedAt.Before(now.Add(-since)) {
			sessions = append(sessions, s)
		}
	}
	tasks, err := e.st.ListTasks(ctx, store.TaskFilter{})
	if err != nil {
		return nil, err
	}
	byID := make(map[string]model.Task, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
	}
	return restorePlan(sessions, panes, byID, now, since), nil
}

// restoreServer is what the plan knows of the answering tmux server.
type restoreServer struct {
	panes  map[string]tmux.Pane
	start  time.Time // the latest ServerStart listed
	socket string    // the first non-empty socket listed
}

func restoreNewServer(live []tmux.Pane) restoreServer {
	srv := restoreServer{panes: make(map[string]tmux.Pane, len(live))}
	for _, p := range live {
		srv.panes[p.ID] = p
		if p.ServerStart.After(srv.start) {
			srv.start = p.ServerStart
		}
		if srv.socket == "" {
			srv.socket = p.Socket
		}
	}
	return srv
}

// otherServer reports whether s was recorded on another tmux server: the
// shared rule, fleet.OtherSocket.
func (srv restoreServer) otherServer(s model.Session) bool {
	return fleet.OtherSocket(srv.socket, s)
}

// live reports whether s runs in a pane of this server: it is not ended,
// its pane is listed, it is not recorded on another server, and it did not
// start before the server did (a restarted server reuses pane ids).
func (srv restoreServer) live(s model.Session) bool {
	if s.State == model.StateEnded || s.Pane == "" || srv.otherServer(s) {
		return false
	}
	if _, ok := srv.panes[s.Pane]; !ok {
		return false
	}
	return s.StartedAt.IsZero() || !s.StartedAt.Before(srv.start)
}

// openPane reports whether pane is listed and was opened at or after at,
// in this server's lifetime.
func (srv restoreServer) openPane(pane string, at time.Time) bool {
	p, ok := srv.panes[pane]
	return ok && pane != "" && !at.IsZero() && !at.Before(p.ServerStart)
}

// restorePlan classifies every session that is not ended (section 4.10, step 3). It reads the file system
// (directories and transcripts) and nothing else. Sessions classed "live" are left out.
//
// With Ruling 117 an ended session whose end reason is "other" (tmux went
// down) is classified too; one with no recorded reason is shown as skipped
// ("ended"), and one the user ended (any other reason) is left out. Among
// the sessions of one task the one in the task's own pane is preferred,
// then the most recently active, and none is restored while the task's
// window is still open.
func restorePlan(sessions []model.Session, live []tmux.Pane, tasks map[string]model.Task, now time.Time, since time.Duration) []restoreRow {
	srv := restoreNewServer(live)
	rows := make([]restoreRow, 0, len(sessions))
	for _, s := range sessions {
		r := restoreClassify(s, srv, tasks, now, since)
		if r.Action != restoreLive && r.Action != restoreExited {
			rows = append(rows, r)
		}
	}
	slices.SortStableFunc(rows, func(a, b restoreRow) int { return b.LastActive.Compare(a.LastActive) })
	restoreSupersede(rows, srv)
	n := 0
	for i := range rows {
		if rows[i].Action != restoreDo {
			continue
		}
		if n++; n > restoreMax {
			rows[i].Action, rows[i].Detail = restoreOverLimit, "over the limit; run again"
		}
	}
	return rows
}

// restoreCheck is one class of section 4.10's table: the action and detail
// of a session it skips, or "" when it does not apply.
type restoreCheck func(r restoreRow, srv restoreServer, now time.Time, since time.Duration) (restoreAction, string)

// restoreChecks are the classes, in the table's order.
var restoreChecks = []restoreCheck{
	restoreCheckEnded, restoreCheckLive, restoreCheckServer, restoreCheckOutside, restoreCheckClosed, restoreCheckTeammate,
	restoreCheckAgent, restoreCheckID, restoreCheckPaused, restoreCheckDir, restoreCheckTranscript, restoreCheckAge,
}

// restoreClassify is one session's row before the per-task and limit steps.
func restoreClassify(s model.Session, srv restoreServer, tasks map[string]model.Task, now time.Time, since time.Duration) restoreRow {
	r := restoreRow{Session: s, Action: restoreDo}
	if t, ok := tasks[s.TaskID]; ok && s.TaskID != "" && t.Status != model.TaskDropped {
		r.Task, r.Linked = t, true
	}
	if fi, err := os.Lstat(s.TranscriptPath); err == nil && fi.Mode().IsRegular() && filepath.IsAbs(s.TranscriptPath) {
		r.LastActive = fi.ModTime()
	}
	for _, check := range restoreChecks {
		if a, d := check(r, srv, now, since); a != "" {
			r.Action, r.Detail = a, d
			break
		}
	}
	return r
}

func restoreCheckEnded(r restoreRow, _ restoreServer, _ time.Time, _ time.Duration) (restoreAction, string) {
	switch {
	case r.Session.State != model.StateEnded || r.Session.EndReason == "other":
		return "", ""
	case r.Session.EndReason == "":
		// Until the hook records end reasons, every ended row has none: it
		// may be the user's own exit, so it is never restored (Ruling 117).
		return restoreEnded, "ended without a recorded reason (perhaps your own exit)"
	}
	return restoreExited, ""
}

func restoreCheckLive(r restoreRow, srv restoreServer, _ time.Time, _ time.Duration) (restoreAction, string) {
	if srv.live(r.Session) {
		return restoreLive, ""
	}
	return "", ""
}

func restoreCheckServer(r restoreRow, srv restoreServer, _ time.Time, _ time.Duration) (restoreAction, string) {
	if srv.otherServer(r.Session) {
		return restoreOtherServer, "on another tmux server (" + sanitize.Line(filepath.Base(r.Session.Socket), 60) + ")"
	}
	return "", ""
}

func restoreCheckOutside(r restoreRow, _ restoreServer, _ time.Time, _ time.Duration) (restoreAction, string) {
	if r.Session.Pane == "" {
		return restoreOutside, "it ran outside tmux"
	}
	return "", ""
}

// restoreCheckClosed skips, unless --include-closed, a session that started at or after the listed server
// did and is not on another server (restoreCheckServer comes first): its pane closed while the server it ran
// on kept running, because the user closed it (or killed it from the dashboard), not because tmux went down
// (Ruling P4-R7). Claude Code reports "other" either way, so the end reason cannot tell.
//
// A session that is not Claude Code's is left to restoreCheckAgent, whose
// skip names the agent's own resume command.
func restoreCheckClosed(r restoreRow, srv restoreServer, _ time.Time, _ time.Duration) (restoreAction, string) {
	s := r.Session
	if restoreIncludeClosed || srv.start.IsZero() || s.StartedAt.IsZero() || s.StartedAt.Before(srv.start) ||
		s.Agent != model.AgentClaude {
		return "", ""
	}
	return restoreClosedHere, "its pane was closed while tmux kept running; reopen it with claude --resume " +
		sanitize.Line(s.ID, 60) + " in " + sanitize.Line(restoreDir(r), 300) + ", or run curia restore --include-closed"
}

func restoreCheckTeammate(r restoreRow, _ restoreServer, _ time.Time, _ time.Duration) (restoreAction, string) {
	if r.Session.TeamRole == model.RoleTeammate {
		return restoreTeammate, "a teammate: its lead respawns its team"
	}
	return "", ""
}

// restoreCheckAgent skips a session that is not Claude Code's: restore
// resumes a session only with its own agent, and Codex resume is unverified
// until probe X1 (Ruling P5-R8), so a Codex session is skipped and
// restorePrintByHand says what to run. It also skips a Claude Code session
// of a task whose stored agent is now another: curia launch would start
// that agent on this session's id.
func restoreCheckAgent(r restoreRow, _ restoreServer, _ time.Time, _ time.Duration) (restoreAction, string) {
	switch {
	case r.Session.Agent == model.AgentCodex:
		return restoreNotClaude, "a Codex session: restore does not resume Codex sessions yet (see below)"
	case r.Session.Agent != model.AgentClaude:
		return restoreNotClaude, "not a Claude Code session"
	case r.Linked && spawnStoredAgent(r.Task) != model.AgentClaude:
		return restoreOtherAgent, r.Task.ID + " now runs " + sanitize.Line(spawnStoredAgent(r.Task), 20) +
			", not this session's Claude Code"
	}
	return "", ""
}

// restorePrintByHand prints one line for each Codex session the plan skips:
// why, and what the user can run by hand in its directory.
func restorePrintByHand(w io.Writer, rows []restoreRow) {
	for _, r := range rows {
		if r.Action != restoreNotClaude || r.Session.Agent != model.AgentCodex {
			continue
		}
		why := "skipped " + spawnShortID(r.Session.ID) + ": a Codex session, not restored until Codex resume is verified; "
		if !restoreSessionID.MatchString(r.Session.ID) {
			_, _ = fmt.Fprintln(w, why+"its id is not one codex resume takes")
			continue
		}
		dir := cmp.Or(sanitize.Line(restoreDir(r), restorePathMax), "its directory")
		_, _ = fmt.Fprintln(w, why+"to resume it by hand, run in "+dir+": codex resume "+r.Session.ID)
	}
}

func restoreCheckID(r restoreRow, _ restoreServer, _ time.Time, _ time.Duration) (restoreAction, string) {
	if !restoreSessionID.MatchString(r.Session.ID) {
		return restoreBadID, "not a Claude Code session id"
	}
	return "", ""
}

func restoreCheckPaused(r restoreRow, _ restoreServer, _ time.Time, _ time.Duration) (restoreAction, string) {
	if r.Linked && !r.Task.PausedAt.IsZero() {
		return restorePaused, "paused; use curia resume " + r.Task.ID
	}
	return "", ""
}

func restoreCheckDir(r restoreRow, _ restoreServer, _ time.Time, _ time.Duration) (restoreAction, string) {
	dir := restoreDir(r)
	if r.Linked {
		own := cmp.Or(r.Session.Worktree, r.Session.Cwd)
		if own == "" || filepath.Clean(own) != filepath.Clean(dir) {
			return restoreNoDir, "not in " + r.Task.ID + "'s worktree"
		}
	}
	if !restoreSafePath(dir) {
		return restoreNoDir, "its directory's path cannot be given to tmux"
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return restoreNoDir, "its directory is gone or not a directory"
	}
	return "", ""
}

func restoreCheckTranscript(r restoreRow, _ restoreServer, _ time.Time, _ time.Duration) (restoreAction, string) {
	if r.LastActive.IsZero() {
		return restoreNoTranscript, "it has no transcript file"
	}
	return "", ""
}

func restoreCheckAge(r restoreRow, _ restoreServer, now time.Time, since time.Duration) (restoreAction, string) {
	if r.LastActive.Before(now.Add(-since)) {
		return restoreTooOld, "last active " + ui.Age(r.LastActive, now) + " ago; use --since"
	}
	return "", ""
}

// restoreSupersede keeps at most one session per linked task, none while
// the task's window is still open: the one in the task's own pane, else
// the most recently active (rows are sorted most recent first).
func restoreSupersede(rows []restoreRow, srv restoreServer) {
	inPane := func(r restoreRow) bool { return r.Task.Pane != "" && r.Session.Pane == r.Task.Pane }
	chosen := map[string]int{}
	for i, r := range rows {
		if r.Action != restoreDo || !r.Linked {
			continue
		}
		if t := r.Task; srv.openPane(t.Pane, t.LaunchedAt) {
			rows[i].Action, rows[i].Detail = restoreSuperseded, t.ID+"'s window "+sanitize.Line(t.Pane, 20)+" is still open"
			continue
		}
		if j, seen := chosen[r.Task.ID]; !seen || (inPane(r) && !inPane(rows[j])) {
			chosen[r.Task.ID] = i
		}
	}
	for i, r := range rows {
		if r.Action == restoreDo && r.Linked && chosen[r.Task.ID] != i {
			rows[i].Action, rows[i].Detail = restoreSuperseded, "another session of "+r.Task.ID+" is restored instead"
		}
	}
}

// restoreDir is the directory a row's window starts in: the task's
// worktree, or the session's cwd.
func restoreDir(r restoreRow) string {
	if r.Linked {
		return r.Task.Worktree
	}
	return r.Session.Cwd
}

// restoreSafePath reports whether p is an absolute, clean path that tmux
// takes as one argument: valid UTF-8, no rune below 0x20 or DEL, not ending
// in ";".
func restoreSafePath(p string) bool {
	if p == "" || len(p) > restorePathMax || !filepath.IsAbs(p) || filepath.Clean(p) != p ||
		!utf8.ValidString(p) || strings.HasSuffix(p, ";") {
		return false
	}
	return !strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// restoreName is s with every run of runes outside A-Za-z0-9_- replaced by
// one "_", cut to 40, or fallback when that is empty.
func restoreName(s, fallback string) string {
	n := restoreNameRun.ReplaceAllString(s, "_")
	if len(n) > 40 {
		n = n[:40]
	}
	return cmp.Or(n, fallback)
}

// restoreTarget is the tmux session and window name of a row's window.
func restoreTarget(r restoreRow) (session, window string) {
	if r.Linked {
		return task.SessionName(r.Task.Repo), r.Task.ID
	}
	base := filepath.Base(filepath.Clean(cmp.Or(r.Session.Repo, r.Session.Cwd, ".")))
	if base == "." || base == string(filepath.Separator) {
		base = ""
	}
	return restoreName(r.Session.TmuxSession, "curia"), restoreName(base, "agent")
}

// restoreSpec is the window for one row whose Action is "restore" (section 4.10, step 5).
//
// Only a Claude Code session is resumed, and a linked one only when its
// task's stored agent is Claude Code too, so the task's stored model is
// Claude Code's. Every field comes from a validated value: the session id against the
// UUID pattern, the task id by task.ValidID, the model by task.ValidModel
// (a stored model that fails is left out), names by restoreName, claude and
// bin absolute paths, the directory an absolute clean path os.Stat showed
// to be a directory. No permission flag is ever passed.
func restoreSpec(row restoreRow, bin, claude string) (tmux.WindowSpec, error) {
	id := row.Session.ID
	if err := restoreSpecAgent(row); err != nil {
		return tmux.WindowSpec{}, err
	}
	dir := restoreDir(row)
	if fi, err := os.Stat(dir); !restoreSafePath(dir) || err != nil || !fi.IsDir() {
		return tmux.WindowSpec{}, fmt.Errorf("%s is not a directory; no window opened", sanitize.Line(dir, 300))
	}
	session, name := restoreTarget(row)
	if !row.Linked {
		if claude == "" || !restoreSafePath(claude) {
			return tmux.WindowSpec{}, errors.New("claude is not on PATH")
		}
		return tmux.WindowSpec{Session: session, Name: name, Dir: dir, Command: []string{claude, "--resume", id}}, nil
	}
	t := row.Task
	if !task.ValidID(t.ID) {
		return tmux.WindowSpec{}, fmt.Errorf("%q is not a task id", sanitize.Line(t.ID, 20))
	}
	if bin == "" || !restoreSafePath(bin) {
		return tmux.WindowSpec{}, errors.New("the curia binary was not found")
	}
	command := []string{bin, "launch", t.ID, "--resume", id}
	if task.ValidModel(t.AgentModel) {
		command = append(command, "--model", t.AgentModel)
	}
	if t.Team {
		command = append(command, "--team")
	}
	return tmux.WindowSpec{Session: session, Name: name, Dir: dir, Env: []string{"CURIA_TASK_ID=" + t.ID}, Command: command}, nil
}

// restoreSpecAgent refuses a row restoreSpec may not build a window for: a
// session that is not Claude Code's, a linked one whose task's stored agent
// is not the session's, or an id that is not a Claude Code session id.
func restoreSpecAgent(row restoreRow) error {
	switch {
	case row.Session.Agent != model.AgentClaude:
		return errors.New("not a Claude Code session: restore resumes only Claude Code sessions")
	case row.Linked && spawnStoredAgent(row.Task) != row.Session.Agent:
		return fmt.Errorf("%s now runs another agent than this session's", sanitize.Line(row.Task.ID, 20))
	case !restoreSessionID.MatchString(row.Session.ID):
		return errors.New("not a Claude Code session id")
	}
	return nil
}

// restoreTodo is the rows to restore, in plan order.
func restoreTodo(rows []restoreRow) []restoreRow {
	var todo []restoreRow
	for _, r := range rows {
		if r.Action == restoreDo {
			todo = append(todo, r)
		}
	}
	return todo
}

// restorePrintPlan prints the plan as a table, every cell cleaned.
func restorePrintPlan(w io.Writer, rows []restoreRow, now time.Time) {
	if len(rows) == 0 {
		return
	}
	cell := func(s string, limit int) string { return ui.Cell(sanitize.Line(s, limit)) }
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SESSION\tWINDOW\tTASK\tREPO@BRANCH\tLAST ACTIVE\tACTION")
	for _, r := range rows {
		where, action, active := "", "skip: "+r.Detail, ""
		if r.Action == restoreDo {
			session, name := restoreTarget(r)
			where, action = session+":"+name, "restore"
		}
		if !r.LastActive.IsZero() {
			active = ui.Age(r.LastActive, now) + " ago"
		}
		repo := ""
		if r.Session.Repo != "" || r.Session.Branch != "" {
			repo = ui.RepoName(r.Session.Repo) + "@" + r.Session.Branch
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", cell(spawnShortID(r.Session.ID), 8), cell(where, 90),
			cell(r.Session.TaskID, 20), cell(repo, 80), cell(active, 20), cell(action, 120))
	}
	_ = tw.Flush()
}

// restoreConfirm is step 4's question: --yes, or "y" typed on a terminal.
func restoreConfirm(e restoreEnv, n int) (bool, error) {
	if restoreYes {
		return true, nil
	}
	if !restoreIsTerminal() {
		return false, errors.New("not on a terminal, so restore cannot ask: pass --yes to restore without asking, or --dry-run to only look")
	}
	noun := "sessions"
	if n == 1 {
		noun = "session"
	}
	_, _ = fmt.Fprintf(e.out, "restore %d %s? [y/N] ", n, noun)
	line, _ := bufio.NewReader(io.LimitReader(e.in, 64)).ReadString('\n')
	if strings.TrimSpace(line) == "y" {
		return true, nil
	}
	_, err := fmt.Fprintln(e.out, "nothing restored")
	return false, err
}

// restoreOutcome is what became of one window restore opened.
type restoreOutcome int

const (
	restorePending restoreOutcome = iota // no word yet
	restoreBack                          // the session reported live in the window
	restoreClosed                        // the window closed before the session started
)

// restoreLaunch is one window restore opened for a row.
type restoreLaunch struct {
	row    restoreRow
	pane   tmux.Pane
	opened time.Time
	state  restoreOutcome
	newID  string // the session id the resume reported under, when not the row's
}

// restoreTools are the programs the windows run, "" when not found.
type restoreTools struct{ bin, claude string }

// restoreFindTools looks for curia (when a linked row is to be restored)
// and claude (when an unlinked one is), once each.
func restoreFindTools(todo []restoreRow) restoreTools {
	var t restoreTools
	linked := slices.ContainsFunc(todo, func(r restoreRow) bool { return r.Linked })
	unlinked := slices.ContainsFunc(todo, func(r restoreRow) bool { return !r.Linked })
	if linked {
		if b, err := restoreBin(); err == nil {
			t.bin = b
		}
	}
	if unlinked {
		if c, err := restoreLookPath("claude"); err == nil {
			t.claude = c
		}
	}
	return t
}

// restoreExecute is steps 5 to 8: one window per row to restore, the wait,
// the rows restore ends, the team notices and one line per session.
func restoreExecute(ctx context.Context, e restoreEnv, rows, todo []restoreRow) error {
	tools := restoreFindTools(todo)
	var launches []*restoreLaunch
	failed := 0
	for _, r := range todo {
		l, err := restoreOpen(ctx, e, r, tools)
		if err != nil {
			_, _ = fmt.Fprintf(e.out, "could not open a window for %s: %s\n", spawnShortID(r.Session.ID), sanitize.Line(err.Error(), 400))
			failed++
			continue
		}
		launches = append(launches, l)
	}
	restoreAwait(ctx, e, launches)
	panes, perr := restoreListPanes(ctx, e.tm, subprocessTimeout)
	srv := restoreNewServer(panes)
	back := 0
	for _, l := range launches {
		_, _ = fmt.Fprintln(e.out, restoreLine(l))
		if l.state != restoreBack {
			failed++
			continue
		}
		back++
		if perr != nil {
			_, _ = fmt.Fprintln(e.out, "tmux did not list its panes, so restore ended no other row")
		} else {
			restoreEndOthers(ctx, e, rows, l, srv)
			restoreKeepUnseen(ctx, e, l, fleet.Socket(panes))
		}
		if l.row.Session.TeamRole == model.RoleLead {
			restoreNotice(ctx, e, l)
		}
	}
	if back > 0 {
		_, _ = fmt.Fprintln(e.out, "Restore itself typed nothing into the new windows. A session that had hit a usage limit "+
			"may open a dialog by itself (/rate-limit-options): answer it in its window.")
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d sessions did not come back", failed, len(todo))
	}
	return nil
}

// restoreOpen opens row's window. Right before, it checks that the row is
// as planned (nothing resumed it meanwhile), that a task's worktree is
// still the task's (taking the task's launch claim), and, in restoreSpec,
// that the directory exists: tmux starts a window whose directory is
// missing in the home directory, and claude --resume works from anywhere.
func restoreOpen(ctx context.Context, e restoreEnv, r restoreRow, tools restoreTools) (*restoreLaunch, error) {
	if !r.Linked {
		return restoreOpenUnlinked(ctx, e, r, tools)
	}
	cur, found, err := e.st.GetSession(ctx, r.Session.ID)
	if err != nil {
		return nil, err
	}
	if !found || !restoreAsPlanned(cur, r.Session) {
		return nil, errRestoreChanged
	}
	claimed, err := restoreClaimTask(ctx, e.st, r.Task)
	if err != nil {
		return nil, err
	}
	r.Task = claimed
	l, err := restoreOpenLinked(ctx, e, r, tools)
	if err != nil {
		return nil, spawnRelease(ctx, e.st, claimed, err)
	}
	return l, nil
}

// errRestoreChanged refuses a row that is no longer as the plan read it.
var errRestoreChanged = errors.New("it changed since the plan (it may be running again); run curia restore again")

// restoreAsPlanned reports whether cur is still the row the plan read.
func restoreAsPlanned(cur, planned model.Session) bool {
	return cur.State == planned.State && cur.Pane == planned.Pane && cur.UpdatedAt.Equal(planned.UpdatedAt)
}

// restoreOpenUnlinked opens the window of a row with no task. In the update that re-checks the row it is set
// ended with restoreEndReason, so that until the resumed session's SessionStart revives it (while its window
// sits at a dialog, say) no second restore offers it again (I6); a linked row is guarded by its task's pane.
// When the window cannot be opened the row is put back as it was.
func restoreOpenUnlinked(ctx context.Context, e restoreEnv, r restoreRow, tools restoreTools) (*restoreLaunch, error) {
	spec, err := restoreSpec(r, tools.bin, tools.claude)
	if err != nil {
		return nil, err
	}
	opened, planned := clock(), r.Session
	changed := false
	err = e.st.UpdateSession(ctx, planned.ID, func(cur model.Session, found bool) model.Session {
		if changed = !found || !restoreAsPlanned(cur, planned); changed {
			return cur
		}
		cur.State, cur.EndReason, cur.UpdatedAt = model.StateEnded, restoreEndReason, opened
		return cur
	})
	switch {
	case err != nil:
		return nil, err
	case changed:
		return nil, errRestoreChanged
	}
	pane, err := e.tm.NewWindow(ctx, spec)
	if err != nil {
		restorePutBack(ctx, e, planned, opened)
		return nil, fmt.Errorf("open the window: %w", err)
	}
	return &restoreLaunch{row: r, pane: pane, opened: opened}, nil
}

// restorePutBack gives row planned back its state, end reason and update time, unless something changed it
// after restoreOpenUnlinked ended it at opened. A failure is logged.
func restorePutBack(ctx context.Context, e restoreEnv, planned model.Session, opened time.Time) {
	err := e.st.UpdateSession(context.WithoutCancel(ctx), planned.ID, func(cur model.Session, found bool) model.Session {
		if found && cur.State == model.StateEnded && cur.EndReason == restoreEndReason && cur.UpdatedAt.Equal(opened) {
			cur.State, cur.EndReason, cur.UpdatedAt = planned.State, planned.EndReason, planned.UpdatedAt
		}
		return cur
	})
	if err != nil {
		e.log.Warn("restore: put a row back", "session", planned.ID, "err", err.Error())
	}
}

// restoreClaimTask checks that t's worktree is still its worktree on its
// branch and takes the task's launch claim (store.ClaimLaunch, against the
// pane the plan read), so no curia spawn of the task runs meanwhile.
func restoreClaimTask(ctx context.Context, st *store.Store, t model.Task) (model.Task, error) {
	ours, err := worktreeIsOurs(ctx, t.Repo, t.Worktree, t.Branch)
	if err != nil {
		return model.Task{}, err
	}
	if !ours {
		return model.Task{}, fmt.Errorf("%s is missing or no longer %s's worktree on %s; no window opened",
			sanitize.Line(t.Worktree, 300), t.ID, sanitize.Line(t.Branch, 80))
	}
	claimed, err := st.ClaimLaunch(ctx, t.ID, t.Pane, clock(), restoreHold)
	if errors.Is(err, store.ErrTaskBusy) {
		return model.Task{}, fmt.Errorf("%s was launched less than %v ago (a curia spawn may be starting it): wait, then run curia restore again", t.ID, restoreHold)
	}
	return claimed, err
}

// restoreOpenLinked opens the window of a linked row whose task r.Task is
// claimed, and records the pane on the task (spawnLink, which closes the
// window again when that fails).
func restoreOpenLinked(ctx context.Context, e restoreEnv, r restoreRow, tools restoreTools) (*restoreLaunch, error) {
	spec, err := restoreSpec(r, tools.bin, tools.claude)
	if err != nil {
		return nil, err
	}
	pane, err := e.tm.NewWindow(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("open the window: %w", err)
	}
	t, err := spawnLink(ctx, e.st, e.tm, r.Task, pane, spawnOptions{Team: r.Task.Team, Model: r.Task.AgentModel})
	if err != nil {
		return nil, err
	}
	r.Task = t
	return &restoreLaunch{row: r, pane: pane, opened: t.LaunchedAt}, nil
}

// restoreAwait is step 6: every restorePoll for at most restoreWait it
// looks for each window's session, until every window has an outcome.
func restoreAwait(ctx context.Context, e restoreEnv, launches []*restoreLaunch) {
	deadline := clock().Add(restoreWait)
	for len(launches) > 0 && ctx.Err() == nil {
		panes, perr := restoreListPanes(ctx, e.tm, subprocessTimeout)
		sessions, err := e.st.ListSessions(ctx, store.ListFilter{})
		if err != nil {
			e.log.Warn("restore: list sessions", "err", err.Error())
		}
		pending := 0
		for _, l := range launches {
			if l.state == restorePending {
				restoreCheckBack(l, sessions, panes, perr == nil)
			}
			if l.state == restorePending {
				pending++
			}
		}
		if pending == 0 || !clock().Before(deadline) {
			return
		}
		restoreSleep(restorePoll)
	}
}

// restoreCheckBack sets l's outcome: back when a live session started in
// its pane after the window opened (its own id, or a new one), closed when
// tmux listed its panes and the window's is gone.
func restoreCheckBack(l *restoreLaunch, sessions []model.Session, panes []tmux.Pane, listed bool) {
	for _, s := range sessions {
		if s.Pane != l.pane.ID || fleet.OtherSocket(l.pane.Socket, s) || s.State == model.StateStale || s.StartedAt.Before(l.opened) {
			continue
		}
		l.state = restoreBack
		if s.ID == l.row.Session.ID {
			l.newID = ""
			return
		}
		l.newID = s.ID
	}
	if l.state == restorePending && listed && !slices.ContainsFunc(panes, func(p tmux.Pane) bool { return p.ID == l.pane.ID }) {
		l.state = restoreClosed
	}
}

// restoreLine is step 8's line for one window.
func restoreLine(l *restoreLaunch) string {
	short, where := spawnShortID(l.row.Session.ID), spawnPaneName(l.pane)
	switch l.state {
	case restoreBack:
		if l.newID != "" {
			return fmt.Sprintf("restored %s in %s as session %s (the resume gave it a new id)", short, where, spawnShortID(l.newID))
		}
		return fmt.Sprintf("restored %s in %s", short, where)
	case restoreClosed:
		return fmt.Sprintf("%s: the window closed without the session starting. Run claude --resume %s in %s to see why; Curia does not retry",
			short, l.row.Session.ID, sanitize.Line(restoreDir(l.row), 300))
	}
	return fmt.Sprintf("%s did not report within %v; look at %s: it may be waiting at a dialog (such as the trust dialog). "+
		"Answer it there rather than running curia restore again", short, restoreWait, where)
}

// restoreKeepUnseen gives a restored session back the "not looked at" it had before the crash: the resumed
// session's SessionStart marks it seen, so a task's review-ready (blue) marker, or a red one, would not come
// back (live check defect 6). When the planned row had not been looked at since its last activity (its
// transcript's last write: reconcile's stale mark moved updated_at), the live row's seen time is cleared,
// unless it was looked at after the resume, and its window's marker is applied again. A failure is logged.
func restoreKeepUnseen(ctx context.Context, e restoreEnv, l *restoreLaunch, socket string) {
	planned := l.row.Session
	if !planned.SeenAt.Before(l.row.LastActive) {
		return
	}
	id := cmp.Or(l.newID, planned.ID)
	var window string
	err := e.st.UpdateSession(ctx, id, func(cur model.Session, found bool) model.Session {
		if found && cur.Pane == l.pane.ID && !cur.SeenAt.After(cur.StartedAt) {
			cur.SeenAt, window = time.Time{}, cur.TmuxWindow
		}
		return cur
	})
	if err == nil && window != "" {
		actx, cancel := context.WithTimeout(ctx, subprocessTimeout)
		defer cancel()
		if err = attention.Apply(actx, e.tm, e.st, window, socket); errors.Is(err, tmux.ErrNoWindow) {
			err = nil
		}
	}
	if err != nil {
		e.log.Warn("restore: keep a restored session unseen", "session", id, "err", err.Error())
	}
}

// restoreEndOthers sets ended the rows a restored session replaces: its old
// row when the resume reported under a new id, the other sessions of its
// task, and, for a team lead, its teammates. Each is re-checked in its
// update and left alone while it is live on the server.
func restoreEndOthers(ctx context.Context, e restoreEnv, rows []restoreRow, l *restoreLaunch, srv restoreServer) {
	s := l.row.Session
	if l.newID != "" {
		restoreEndRow(ctx, e, s.ID, srv)
	}
	mates := 0
	for _, r := range rows {
		switch {
		case r.Session.ID == s.ID:
		case l.row.Linked && r.Action == restoreSuperseded && r.Linked && r.Task.ID == l.row.Task.ID:
			restoreEndRow(ctx, e, r.Session.ID, srv)
		case s.TeamRole == model.RoleLead && s.TeamID != "" && r.Action == restoreTeammate && r.Session.TeamID == s.TeamID:
			if restoreEndRow(ctx, e, r.Session.ID, srv) {
				mates++
			}
		}
	}
	if mates > 0 {
		_, _ = fmt.Fprintf(e.out, "%s's %d teammates did not survive the restart: their rows are ended\n", spawnShortID(s.ID), mates)
	}
}

// restoreEndRow sets session id ended, with restoreEndReason, unless it is
// live on srv; it reports whether it did.
func restoreEndRow(ctx context.Context, e restoreEnv, id string, srv restoreServer) bool {
	ended := false
	err := e.st.UpdateSession(ctx, id, func(cur model.Session, found bool) model.Session {
		ended = false
		if !found || srv.live(cur) {
			return cur
		}
		cur.State, cur.EndReason, cur.UpdatedAt = model.StateEnded, restoreEndReason, clock()
		ended = true
		return cur
	})
	if err != nil {
		e.log.Warn("restore: end a replaced row", "session", id, "err", err.Error())
		return false
	}
	return ended
}

// restoreNotice is step 7's notice to a restored lead: a Plan 3 message,
// offered once to deliver.Message, whose guards decide whether it is typed.
func restoreNotice(ctx context.Context, e restoreEnv, l *restoreLaunch) {
	lead := cmp.Or(l.newID, l.row.Session.ID)
	short := spawnShortID(lead)
	m, err := e.st.CreateMessage(ctx, model.Message{
		Text: restoreLeadText, Kind: model.KindInstruction, Sender: model.SenderUser, TargetSession: lead,
		TargetTask: l.row.Task.ID, RoutedBy: model.RoutedAddress, Status: model.MsgQueued, CreatedAt: clock(),
	})
	if err != nil {
		_, _ = fmt.Fprintf(e.out, "could not store the team notice for %s: %s\n", short, sanitize.Line(err.Error(), 300))
		return
	}
	out, err := deliver.Message(ctx, e.st, e.tm, e.log, m.ID, clock())
	if err != nil {
		_, _ = fmt.Fprintf(e.out, "team notice %s for %s: %s\n", m.ID, short, sanitize.Line(err.Error(), 300))
		return
	}
	_, _ = fmt.Fprintf(e.out, "team notice for %s: %s\n", short, sayOutcomeLine(out))
	_, _ = fmt.Fprintln(e.out, "Restore never types the notice itself: it is typed only into an empty prompt (a resumed "+
		"session shows \"← for agents\" for its first seconds), otherwise the lead gets it at its next stop or by "+
		"check_messages. Prompt the lead if it stays idle.")
}
