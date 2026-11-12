package decision

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
)

// Limits of the delivered wording (plan section 4.9).
const (
	typedLabel    = 60   // runes of the option label in a typed line
	typedText     = 200  // runes of free text, and of reasoning, in a typed line
	maxStopReason = 4000 // runes of a whole stop reason
)

const (
	stopHeader = "Curia: decisions answered while you were working."
	alsoPrefix = "Also answered: "
	alsoSuffix = ". Call check_decision for each."
)

// idOf is d's id, rebuilt from its number when the stored id is not a
// valid one, so a delivered line always starts "Decision D-<n>".
func idOf(d model.Decision) string {
	if ValidID(d.ID) {
		return d.ID
	}
	return "D-" + strconv.FormatInt(d.N, 10)
}

// labelOf is the label of the option d was answered with.
func labelOf(d model.Decision) string {
	if i := optionIndex(d.Options, d.AnswerKey); i >= 0 {
		return d.Options[i].Label
	}
	_, label, _ := strings.Cut(d.Answer, ": ")
	return label
}

// join joins the non-empty ones of a and b with sep.
func join(sep, a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + sep + b
}

// wording is the fixed sentence of a delivered answer to d (a confirmed
// ticket gets the answered wording). choice is "option B, Postgres",
// "option B [Postgres]" or free text, and may be empty; reasoning may be
// empty. Neither ends in a period. An override says "use option B" for a
// key but gives free text as written, so the text "use the existing queue"
// reads the same in both wordings of section 4.9.
func wording(d model.Decision, choice, reasoning string) string {
	overridden := d.Status == model.StatusOverridden
	var b strings.Builder
	b.WriteString("Decision " + idOf(d))
	switch {
	case overridden && d.AnswerKey != "":
		b.WriteString(" overridden: use " + choice + ", not your assumption")
	case overridden && choice != "":
		b.WriteString(" overridden: " + choice + ", not your assumption")
	case overridden:
		b.WriteString(" overridden")
	case choice != "":
		b.WriteString(" answered: " + choice)
	default:
		b.WriteString(" answered")
	}
	b.WriteString(".")
	if reasoning != "" {
		b.WriteString(" Reasoning: " + reasoning + ".")
	}
	if overridden {
		b.WriteString(" Revert work based on the old assumption, then continue.")
	} else {
		b.WriteString(" Continue.")
	}
	return b.String()
}

// Retraction is the fixed sentence that delivers superseded decision d by
// every channel in place of its answer (Ruling 82): an answer the user
// retired is never handed over as live. It holds no text but d's id, and
// only the characters a typed line may hold.
func Retraction(d model.Decision) string {
	return "Decision " + idOf(d) + ": the user retracted this answer. Do not act on it. " +
		"If you still need a decision, raise the question again with request_decision."
}

// TypedMessage is the one line curia decide types into the asking agent's
// pane for decided ticket d, at most 600 runes. The option key, label, free
// text and reasoning go through sanitize.Keys, so the line holds only
// letters, digits, spaces and ". , : - _ /", has no line break and always
// starts with the fixed words "Decision D-<n>" (Ruling 51). When Keys
// dropped or cut anything, the line ends with " Exact wording: call
// check_decision D-<n>.". A confirmed ticket gets the answered wording, and
// a superseded one the Retraction.
func TypedMessage(d model.Decision) string {
	if !d.SupersededAt.IsZero() {
		return Retraction(d)
	}
	lossy := false
	keys := func(s string, limit int) string {
		out, l := sanitize.Keys(s, limit)
		trimmed := strings.TrimRight(out, ". ")
		// Periods alone leave nothing to type: point at the exact wording.
		lossy = lossy || l || (out != "" && trimmed == "")
		return trimmed
	}
	id := idOf(d)
	var choice string
	if d.AnswerKey != "" {
		choice = join(", ", join(" ", "option", keys(d.AnswerKey, MaxKey)), keys(labelOf(d), typedLabel))
	} else {
		choice = keys(d.Answer, typedText)
	}
	msg := wording(d, choice, keys(d.AnswerReasoning, typedText))
	if lossy {
		msg += " Exact wording: call check_decision " + id + "."
	}
	return msg
}

// flat cleans s with sanitize.Text and joins its lines with one space.
func flat(s string, limit int) string {
	return strings.Join(strings.Fields(sanitize.Text(s, limit)), " ")
}

// dropPeriod drops the trailing periods of s before the template adds its
// own, unless they are all there is: then s is the answer as written.
func dropPeriod(s string) string {
	if t := strings.TrimRight(s, ". "); t != "" {
		return t
	}
	return s
}

// stopLine is the line of d in a stop reason: the typed-line wording with
// the full label in square brackets after the key (kept as is, brackets
// and all) and the full free text and reasoning, each flattened to one
// line (Ruling 59). A superseded decision gets the Retraction.
func stopLine(d model.Decision) string {
	if !d.SupersededAt.IsZero() {
		return Retraction(d)
	}
	var choice string
	if d.AnswerKey != "" {
		choice = join(" ", "option", flat(d.AnswerKey, MaxKey))
		if label := flat(labelOf(d), MaxLabel); label != "" {
			choice += " [" + label + "]"
		}
	} else {
		choice = dropPeriod(flat(d.Answer, MaxAnswerText))
	}
	reasoning := dropPeriod(flat(d.AnswerReasoning, MaxReasoning))
	return wording(d, choice, reasoning)
}

// StopReason is the reason curia hook Stop gives when it blocks a stop to
// deliver the answers ds: a header line, then one line per decision, at
// most 4000 runes in all. Decisions that do not fit are named in a last
// line "Also answered: D-120, D-121. Call check_decision for each.". It is
// "" when ds is empty.
func StopReason(ds []model.Decision) string {
	if len(ds) == 0 {
		return ""
	}
	ids := make([]string, len(ds))
	for i, d := range ds {
		ids[i] = idOf(d)
	}
	lines := []string{stopHeader}
	used := utf8.RuneCountInString(stopHeader)
	for i, d := range ds {
		l := stopLine(d)
		n := 1 + utf8.RuneCountInString(l)
		if used+n+alsoLen(ids[i+1:]) > maxStopReason {
			return strings.Join(append(lines, alsoLine(ids[i:], maxStopReason-used-1)), "\n")
		}
		lines = append(lines, l)
		used += n
	}
	return strings.Join(lines, "\n")
}

// alsoLen is the length of the line, with its line break, that names ids,
// or 0 when there are none. Ids are ASCII, so bytes are runes.
func alsoLen(ids []string) int {
	if len(ids) == 0 {
		return 0
	}
	n := 1 + len(alsoPrefix) + len(alsoSuffix) + 2*(len(ids)-1)
	for _, id := range ids {
		n += len(id)
	}
	return n
}

// alsoLine names ids in an "Also answered" line of at most budget runes.
// Only a batch of several hundred answers needs the budget, and then the
// ids past it are left out.
func alsoLine(ids []string, budget int) string {
	n := len(alsoPrefix) + len(alsoSuffix)
	k := 0
	for ; k < len(ids); k++ {
		add := len(ids[k])
		if k > 0 {
			add += 2
		}
		if n+add > budget {
			break
		}
		n += add
	}
	return alsoPrefix + strings.Join(ids[:k], ", ") + alsoSuffix
}
