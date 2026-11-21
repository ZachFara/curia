package task

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
)

// Limits of review texts.
const (
	MaxSummary     = 2000    // runes
	MaxTestsDetail = 200     // runes
	MaxPRBody      = 8 << 10 // bytes

	maxPRTitle = 120
)

var (
	reviewIDRe = regexp.MustCompile(`^R-[1-9][0-9]{0,8}$`)
	tildeRuns  = regexp.MustCompile(`~{3,}`)
)

// ValidReviewID reports whether id is a review id, R-<n>.
func ValidReviewID(id string) bool { return reviewIDRe.MatchString(id) }

// Completion is complete_task's arguments.
type Completion struct {
	Summary     string
	Tests       model.TestsStatus
	TestsDetail string
}

// NormalizeCompletion cleans the texts, with no cut, so that
// ValidateCompletion can report a text that is too long.
func NormalizeCompletion(c Completion) Completion {
	c.Summary = sanitize.Text(c.Summary, 0)
	c.TestsDetail = sanitize.Line(c.TestsDetail, 0)
	return c
}

// ValidateCompletion reports every problem of a normalized c, in the order
// summary, tests_status, tests_detail.
func ValidateCompletion(c Completion) []Problem {
	var ps []Problem
	if n := utf8.RuneCountInString(c.Summary); n < 1 || n > MaxSummary {
		ps = append(ps, Problem{"summary", "must be 1 to 2000 characters"})
	}
	switch c.Tests {
	case model.TestsPassed, model.TestsFailed, model.TestsNotRun:
	default:
		ps = append(ps, Problem{"tests_status", "must be passed, failed or not_run"})
	}
	if utf8.RuneCountInString(c.TestsDetail) > MaxTestsDetail {
		ps = append(ps, Problem{"tests_detail", "must be at most 200 characters"})
	}
	return ps
}

// ErrBadNote is returned for a review note of the wrong length.
var ErrBadNote = errors.New("task: a review note must be 1 to 600 characters")

// NormalizeNote cleans a change request and checks its length.
func NormalizeNote(note string) (string, error) {
	note = sanitize.Text(note, 0)
	if n := utf8.RuneCountInString(note); n < 1 || n > model.MaxReviewNote {
		return "", ErrBadNote
	}
	return note, nil
}

// NoteMessage is the message that carries a change request. Its fixed words
// hold none of the characters a typed line strips (a colon, a message id:
// Ruling 105), so only the developer's own note can make a typed change
// request "Wording simplified".
func NoteMessage(reviewID, taskID, note string) string {
	return "Changes requested in review " + reviewID + " of " + taskID + ". " + note
}

// PRTitle is the task id and title on one line, without mentions, at most
// 120 runes.
func PRTitle(t model.Task) string {
	title := strings.ReplaceAll(t.Title, "@", "")
	return sanitize.Line(t.ID+": "+title, maxPRTitle)
}

// PRBody is the pull-request description. What the agent wrote, its tests
// detail and its summary, is inside one tilde fence (whose tilde runs are
// cut to two, so neither can close it); outside it only Curia's own words,
// the ids and the tests status (an enum) appear, so no closing keyword,
// mention, link or image of the agent's reaches the rendered page.
func PRBody(t model.Task, r model.Review, decided []model.Decision) string {
	var head strings.Builder
	head.WriteString("Curia task " + t.ID + ", review " + r.ID + ".\n\n")
	head.WriteString("Tests (the agent's report): " + string(r.Tests) + ".\n\n")
	if len(decided) > 0 {
		ids := make([]string, len(decided))
		for i, d := range decided {
			ids[i] = d.ID
		}
		head.WriteString("Decisions on this task: " + strings.Join(ids, ", ") + "\n\n")
	}
	head.WriteString("The agent's tests detail and summary:\n\n~~~~text\n")
	if detail := strings.TrimSpace(sanitize.Line(r.TestsDetail, MaxTestsDetail)); detail != "" {
		head.WriteString("Tests detail: " + tildeRuns.ReplaceAllString(detail, "~~") + "\n\n")
	}
	const tail = "\n~~~~\n"

	summary := tildeRuns.ReplaceAllString(sanitize.Text(r.Summary, MaxSummary), "~~")
	if room := MaxPRBody - head.Len() - len(tail); len(summary) > room {
		if room < 0 {
			room = 0
		}
		summary = summary[:room]
		for !utf8.ValidString(summary) {
			summary = summary[:len(summary)-1]
		}
	}
	return head.String() + summary + tail
}

// ReviewStatusWord is a review status as a sentence says it.
func ReviewStatusWord(s model.ReviewStatus) string {
	return strings.ReplaceAll(sanitize.Line(string(s), 30), "_", " ")
}
