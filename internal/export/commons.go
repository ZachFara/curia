package export

import (
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/ZachFara/curia/internal/decision"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
)

// Entry is the argument object of Agent Commons's propose tool, without the proposing agent's own fields.
type Entry struct {
	Symptom     string            `json:"symptom"`
	Environment map[string]string `json:"environment"`
	Cause       string            `json:"cause"`
	Fix         string            `json:"fix"`
	Domains     []string          `json:"domains"`
}

// ErrNotPublishable is returned for a decision that is not decided, is superseded, or is not of global scope.
var ErrNotPublishable = errors.New("export: not a decided global decision")

// Limits of an entry's strings, in runes.
const (
	maxSymptom = 500
	maxCause   = 1500
	maxFix     = 2000
)

// CommonsEntry maps a decided global decision to an Agent Commons entry. It
// holds no path, task id, session id, repository or branch.
func CommonsEntry(d model.Decision) (Entry, error) {
	if !decision.IsDecided(d.Status) || !d.SupersededAt.IsZero() || d.Scope != model.ScopeGlobal {
		return Entry{}, ErrNotPublishable
	}
	e := Entry{
		Symptom:     "Decision: " + text(d.Question, maxSymptom),
		Environment: map[string]string{"source": "curia", "kind": "decision"},
		Domains:     tagList(d.Tags, 60),
	}

	cause := text(d.Context, maxCause)
	var opts []string
	for i, o := range d.Options {
		if i == maxOptions {
			break
		}
		if key := line(o.Key, 20); key != "" {
			opts = append(opts, key+": "+line(o.Label, 100))
		}
	}
	if len(opts) > 0 {
		if cause != "" {
			cause += "\n\n"
		}
		cause += "Options considered: " + strings.Join(opts, "; ")
	}
	e.Cause = cause

	var fix []string
	if chosen := text(answerText(d), maxFix); chosen != "" {
		if r, _ := utf8.DecodeLastRuneInString(chosen); !strings.ContainsRune(".!?", r) {
			chosen += "."
		}
		fix = append(fix, "Chosen: "+chosen)
	}
	if why := text(d.AnswerReasoning, maxFix); why != "" {
		fix = append(fix, "Reasoning: "+why)
	}
	e.Fix = sanitize.Text(strings.Join(fix, " "), maxFix)
	return e, nil
}

// head is the part of s that can matter for a result of at most runes runes:
// cleaning only removes bytes, so a huge input costs no more than a small one.
func head(s string, runes int) string {
	if len(s) > 4*runes {
		return s[:4*runes]
	}
	return s
}

func text(s string, runes int) string { return sanitize.Text(head(s, runes), runes) }
func line(s string, runes int) string { return sanitize.Line(head(s, runes), runes) }
