package decision

import (
	"errors"
	"time"
	"unicode/utf8"

	"github.com/ZachFara/curia/internal/model"
)

// ErrBadAnswer is returned by Resolve for an answer that does not fit the
// ticket.
var ErrBadAnswer = errors.New("decision: bad answer")

// IsDecided reports whether s is a decided status (answered, confirmed or
// overridden), the only statuses that are precedent.
func IsDecided(s model.DecisionStatus) bool {
	return s == model.StatusAnswered || s == model.StatusConfirmed || s == model.StatusOverridden
}

// Resolve applies answer a to ticket d at now and returns the decided row,
// with Status and Delivery set by plan section 4.3. It checks only the
// answer; the store checks the status and the deadline inside its
// transaction. Free text and reasoning are cleaned as Normalize cleans
// prose, so a leading line break cannot hide a rune too many.
func Resolve(d model.Decision, a model.Answer, now time.Time) (model.Decision, error) {
	if (a.Key == "") == (a.Text == "") {
		return model.Decision{}, ErrBadAnswer
	}
	reasoning := text(a.Reasoning, MaxReasoning)
	if utf8.RuneCountInString(reasoning) > MaxReasoning || (a.Scope != "" && !validScope(a.Scope)) {
		return model.Decision{}, ErrBadAnswer
	}
	if a.Key != "" {
		i := optionIndex(d.Options, a.Key)
		if i < 0 {
			return model.Decision{}, ErrBadAnswer
		}
		d.AnswerKey, d.Answer = a.Key, a.Key+": "+d.Options[i].Label
	} else {
		t := text(a.Text, MaxAnswerText)
		if n := utf8.RuneCountInString(t); n < 1 || n > MaxAnswerText {
			return model.Decision{}, ErrBadAnswer
		}
		d.AnswerKey, d.Answer = "", t
	}
	d.AnswerReasoning = reasoning
	d.AnsweredVia = a.Via
	d.AnsweredAt = now
	if a.Scope != "" {
		d.Scope = a.Scope
	}
	switch {
	case d.Mode != model.ModeAssume:
		d.Status, d.Delivery = model.StatusAnswered, model.DeliveryQueued
	case d.AnswerKey != "" && d.AnswerKey == d.Recommendation:
		d.Status, d.Delivery = model.StatusConfirmed, model.DeliveryNone
	default:
		d.Status, d.Delivery = model.StatusOverridden, model.DeliveryQueued
	}
	if d.SessionID == "" {
		d.Delivery = model.DeliveryNone
	}
	return d, nil
}

// optionIndex is the index of the option whose key is exactly key, or -1.
func optionIndex(opts []model.Option, key string) int {
	for i, o := range opts {
		if o.Key == key {
			return i
		}
	}
	return -1
}
