package route

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ZachFara/curia/internal/message"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/task"
)

// TargetType is what kind of thing a candidate is.
type TargetType string

// Target types.
const (
	TargetSession TargetType = "session"
	TargetTask    TargetType = "task"
	TargetRepo    TargetType = "repo"
	TargetGlobal  TargetType = "global"
)

const labelLimit = 120 // runes of a candidate's label

// Candidate is one place a message can go.
type Candidate struct {
	Key     string // s1, s2, …; the task id; r1, r2, …; "global"
	Type    TargetType
	Ref     string // session:<id>, task:<id>, repo:<path>, global
	Label   string // what the user sees and what the model sees
	Session model.Session
	Task    model.Task
	Repo    string // the repository of any candidate but global
}

// Candidates lists the targets (section 4.10): the live sessions, a
// teammate left out in favor of its lead; the tasks shown as backlog,
// ready, running or needs_decision; the distinct repositories of those; and
// global.
func Candidates(sessions []model.Session, cards []task.Card) []Candidate {
	var out []Candidate
	var repos []string
	seen := map[string]bool{}
	addRepo := func(r string) {
		if r != "" && !seen[r] {
			seen[r] = true
			repos = append(repos, r)
		}
	}
	n := 0
	for _, s := range sessions {
		if s.State == model.StateEnded || s.TeamRole == model.RoleTeammate {
			continue
		}
		n++
		parts := []string{base(s.Repo), s.Branch}
		if s.TaskID != "" {
			parts = append(parts, s.TaskID)
		}
		parts = append(parts, strings.Trim(s.TmuxSession+":"+s.TmuxWindow, ":"), string(s.State))
		out = append(out, Candidate{
			Key: "s" + strconv.Itoa(n), Type: TargetSession, Ref: "session:" + s.ID,
			Label: label(parts...), Session: s, Repo: s.Repo,
		})
		addRepo(s.Repo)
	}
	for _, c := range cards {
		switch c.Status {
		case model.TaskBacklog, model.TaskReady, model.TaskRunning, model.TaskNeedsDecision:
		default:
			continue
		}
		t := c.Task
		out = append(out, Candidate{
			Key: t.ID, Type: TargetTask, Ref: "task:" + t.ID,
			Label: label(t.ID+" "+t.Title, base(t.Repo), string(c.Status)), Task: t, Repo: t.Repo,
		})
		addRepo(t.Repo)
	}
	for i, r := range repos {
		out = append(out, Candidate{
			Key: "r" + strconv.Itoa(i+1), Type: TargetRepo, Ref: "repo:" + r,
			Label: label("repo", base(r)), Repo: r,
		})
	}
	return append(out, Candidate{Key: "global", Type: TargetGlobal, Ref: "global", Label: "global (every repository)"})
}

func base(p string) string {
	if p == "" {
		return ""
	}
	return filepath.Base(p)
}

// label joins the non-empty parts as one cleaned line.
func label(parts ...string) string {
	var keep []string
	for _, p := range parts {
		if p = sanitize.Line(p, labelLimit); p != "" {
			keep = append(keep, p)
		}
	}
	return sanitize.Line(strings.Join(keep, " · "), labelLimit)
}

// Resolve finds the candidates an address names. A task id part wins over
// a name (the name is then ignored). A name is replaced by its alias value
// once (the value is not looked up as an alias again); then, in order: a
// task id, a repository base name (ignoring case), a branch. A repository
// name names that repository's live sessions, or the repository itself
// when it has none; a branch names sessions on it, or tasks when none are.
// Zero means nothing matched; several means the user must pick.
func Resolve(a message.Address, cands []Candidate, aliases map[string]string) []Candidate {
	if a.TaskID != "" {
		return pick(cands, func(c Candidate) bool { return c.Type == TargetTask && c.Key == a.TaskID })
	}
	if a.Name == "" {
		return nil
	}
	name := unalias(a.Name, aliases)
	repoIs := func(c Candidate) bool {
		return c.Repo != "" && strings.EqualFold(filepath.Base(c.Repo), name)
	}
	steps := []func(Candidate) bool{
		func(c Candidate) bool { return c.Type == TargetTask && c.Key == name },
		func(c Candidate) bool { return c.Type == TargetSession && repoIs(c) },
		func(c Candidate) bool { return c.Type == TargetRepo && repoIs(c) },
		func(c Candidate) bool { return c.Type == TargetSession && c.Session.Branch == name },
		func(c Candidate) bool { return c.Type == TargetTask && c.Task.Branch == name },
	}
	for _, f := range steps {
		if r := pick(cands, f); len(r) > 0 {
			return r
		}
	}
	return nil
}

// unalias replaces name by its alias value, once.
func unalias(name string, aliases map[string]string) string {
	if v, ok := aliases[name]; ok {
		return v
	}
	for k, v := range aliases {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return name
}

func pick(cands []Candidate, f func(Candidate) bool) []Candidate {
	var out []Candidate
	for _, c := range cands {
		if f(c) {
			out = append(out, c)
		}
	}
	return out
}

// Target is where a message of one kind finally goes.
type Target struct {
	Session, Task, Repo string
	Global              bool
}

// ErrNoTarget is wrapped by every refusal of Settle.
var ErrNoTarget = errors.New("route: this target cannot take that kind of message")

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrNoTarget, fmt.Sprintf(format, args...))
}

// Settle applies section 4.10's table: what picking c means for a message
// of the given kind. The error's text says why, for the user to read.
func Settle(kind model.MessageKind, c Candidate, cands []Candidate) (Target, error) {
	switch kind {
	case model.KindInstruction:
		return settleInstruction(c, cands)
	case model.KindRule:
		if c.Type == TargetGlobal {
			return Target{Global: true}, nil
		}
		if c.Repo == "" {
			return Target{}, refuse("%s has no repository", c.Key)
		}
		return Target{Repo: c.Repo}, nil
	case model.KindTask:
		if c.Type == TargetGlobal {
			return Target{}, refuse("a task needs a repository, not global")
		}
		if c.Repo == "" {
			return Target{}, refuse("%s has no repository", c.Key)
		}
		return Target{Repo: c.Repo}, nil
	}
	return Target{}, fmt.Errorf("route: unknown message kind %q", kind)
}

func settleInstruction(c Candidate, cands []Candidate) (Target, error) {
	switch c.Type {
	case TargetSession:
		return Target{Session: c.Session.ID}, nil
	case TargetTask:
		for _, x := range cands {
			if x.Type == TargetSession && x.Session.TaskID == c.Task.ID {
				return Target{Session: x.Session.ID, Task: c.Task.ID}, nil
			}
		}
		return Target{}, refuse("%s has no live agent", c.Task.ID)
	case TargetRepo:
		var found []Candidate
		for _, x := range cands {
			if x.Type == TargetSession && x.Repo == c.Repo {
				found = append(found, x)
			}
		}
		if len(found) == 0 {
			return Target{}, refuse("%s has no live agent", base(c.Repo))
		}
		if len(found) > 1 {
			return Target{}, refuse("%s has several live agents; address one of them", base(c.Repo))
		}
		return Target{Session: found[0].Session.ID}, nil
	}
	return Target{}, refuse("an instruction cannot go to global")
}
