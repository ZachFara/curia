// Package task holds the pure rules for tasks: field validation, derived
// names, derived status and the first prompt of a task's agent.
package task

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
)

// Limits.
const (
	MaxTitle = 120
	MaxBody  = 48 << 10 // bytes
	MaxSlug  = 40
	// MaxTags is the most tags a task may hold.
	MaxTags = 8

	maxBranch  = 200
	maxSession = 40
)

var (
	idRe      = regexp.MustCompile(`^T-[1-9][0-9]{0,8}$`)
	modelRe   = regexp.MustCompile(`^[A-Za-z0-9._\[\]-]{1,64}$`)
	branchRe  = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
	tagRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	slugRuns  = regexp.MustCompile(`[^a-z0-9]+`)
	sessionRe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)
)

// Problem is one thing wrong with a task field.
type Problem struct{ Field, Problem string }

// ValidID reports whether id is a task id, T-<n>.
func ValidID(id string) bool { return idRe.MatchString(id) }

// ValidModel reports whether m is an acceptable agent model name.
func ValidModel(m string) bool { return modelRe.MatchString(m) }

// ValidBranch reports whether b is an acceptable base branch name.
func ValidBranch(b string) bool {
	return branchRe.MatchString(b) && !strings.HasPrefix(b, "-") &&
		!strings.HasPrefix(b, "/") && !strings.Contains(b, "..")
}

// Normalize cleans the title, body and tags of t as section 4.3 says. The
// title and tags are cut one past their limit so Validate still sees an
// overlong value.
func Normalize(t model.Task) model.Task {
	t.Title = sanitize.Line(t.Title, MaxTitle+1)
	t.Body = sanitize.Block(t.Body, MaxBody)
	t.Tags = normTags(t.Tags)
	return t
}

func normTags(raw []string) []string {
	if raw == nil {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		s := strings.ToLower(sanitize.Line(r, 33))
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
		if len(out) > MaxTags {
			break
		}
	}
	sort.Strings(out)
	return out
}

// Validate reports every problem of t, in field order. t is already
// normalised. It does not look at the file system.
func Validate(t model.Task) []Problem {
	var ps []Problem
	for _, check := range []func(model.Task) []Problem{
		checkTitle, checkBody, checkRepo, checkBase, checkTags, checkModel,
	} {
		ps = append(ps, check(t)...)
	}
	return ps
}

func one(field, msg string) []Problem { return []Problem{{Field: field, Problem: msg}} }

func checkTitle(t model.Task) []Problem {
	n := utf8.RuneCountInString(t.Title)
	switch {
	case strings.TrimSpace(t.Title) == "":
		return one("title", "is required")
	case n > MaxTitle:
		return one("title", fmt.Sprintf("must be at most %d characters", MaxTitle))
	case strings.ContainsAny(t.Title, "\n\r"):
		return one("title", "must be one line")
	}
	return nil
}

func checkBody(t model.Task) []Problem {
	if len(t.Body) > MaxBody {
		return one("body", fmt.Sprintf("must be at most %d bytes", MaxBody))
	}
	return nil
}

func checkRepo(t model.Task) []Problem {
	if t.Repo == "" || !filepath.IsAbs(t.Repo) || filepath.Clean(t.Repo) != t.Repo {
		return one("repo", "must be an absolute, cleaned path")
	}
	return nil
}

func checkBase(t model.Task) []Problem {
	if t.BaseBranch != "" && !ValidBranch(t.BaseBranch) {
		return one("base_branch", fmt.Sprintf("must be 1 to %d of A-Z, a-z, 0-9, ., _, / and -, not start with - or /, and not contain ..", maxBranch))
	}
	return nil
}

func checkTags(t model.Task) []Problem {
	var ps []Problem
	if len(t.Tags) > MaxTags {
		ps = one("tags", fmt.Sprintf("must hold at most %d tags", MaxTags))
	}
	for _, tag := range t.Tags {
		if !tagRe.MatchString(tag) {
			ps = append(ps, one("tags", fmt.Sprintf("tag %q must be 1 to 32 of a-z, 0-9, _ and -, starting with a letter or digit", tag))...)
		}
	}
	return ps
}

func checkModel(t model.Task) []Problem {
	if t.AgentModel != "" && !ValidModel(t.AgentModel) {
		return one("agent_model", "must be 1 to 64 of A-Z, a-z, 0-9, ., _, [, ] and -")
	}
	return nil
}

// Slug is the title lowercased, every run of runes outside a-z0-9 one "-",
// trimmed of "-" and cut to MaxSlug; "task" when empty.
func Slug(title string) string {
	s := strings.Trim(slugRuns.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if len(s) > MaxSlug {
		s = strings.Trim(s[:MaxSlug], "-")
	}
	if s == "" {
		return "task"
	}
	return s
}

// Branch is the branch name of a task: curia/<id>-<slug>.
func Branch(id, title string) string { return "curia/" + id + "-" + Slug(title) }

// WorktreePath is <dir of repo>/.curia-worktrees/<base of repo>-<id>.
func WorktreePath(repo, id string) string {
	repo = filepath.Clean(repo)
	return filepath.Join(filepath.Dir(repo), ".curia-worktrees", filepath.Base(repo)+"-"+id)
}

// SessionName is the tmux session of a repo: its base name with every run of
// runes outside A-Za-z0-9_- replaced by "_", cut to 40; "curia" when empty.
func SessionName(repo string) string {
	base := filepath.Base(filepath.Clean(repo))
	if base == "/" || base == "." || base == string(filepath.Separator) {
		return "curia"
	}
	s := sessionRe.ReplaceAllString(base, "_")
	if len(s) > maxSession {
		s = s[:maxSession]
	}
	if s == "" {
		return "curia"
	}
	return s
}
