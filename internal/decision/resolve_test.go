package decision

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
)

func openDecision(mode model.Mode, session string) model.Decision {
	return model.Decision{
		N: 117, ID: "D-117", SessionID: session,
		Question: "Store webhook events in Postgres or in the existing Redis?",
		Options: []model.Option{
			{Key: "A", Label: "Postgres table"},
			{Key: "B", Label: "Redis stream"},
		},
		Recommendation: "A",
		Mode:           mode,
		Scope:          model.ScopeRepo,
		Status:         model.StatusOpen,
		Delivery:       model.DeliveryNone,
	}
}

func TestResolveTable(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		mode       model.Mode
		answer     model.Answer
		status     model.DecisionStatus
		delivery   model.Delivery
		answerKey  string
		answerText string
		scope      model.Scope
	}{
		{
			"blocking key", model.ModeBlocking,
			model.Answer{Key: "B"},
			model.StatusAnswered, model.DeliveryQueued, "B", "B: Redis stream", model.ScopeRepo,
		},
		{
			"blocking text", model.ModeBlocking,
			model.Answer{Text: " use the\x1b[1m existing queue \n"},
			model.StatusAnswered, model.DeliveryQueued, "", "use the existing queue", model.ScopeRepo,
		},
		{
			"assume recommended key", model.ModeAssume,
			model.Answer{Key: "A"},
			model.StatusConfirmed, model.DeliveryNone, "A", "A: Postgres table", model.ScopeRepo,
		},
		{
			"assume other key", model.ModeAssume,
			model.Answer{Key: "B"},
			model.StatusOverridden, model.DeliveryQueued, "B", "B: Redis stream", model.ScopeRepo,
		},
		{
			"assume text", model.ModeAssume,
			model.Answer{Text: "Postgres, with a view"},
			model.StatusOverridden, model.DeliveryQueued, "", "Postgres, with a view", model.ScopeRepo,
		},
		{
			"scope given", model.ModeBlocking,
			model.Answer{Key: "A", Scope: model.ScopeGlobal},
			model.StatusAnswered, model.DeliveryQueued, "A", "A: Postgres table", model.ScopeGlobal,
		},
	}
	for _, tt := range tests {
		for _, session := range []string{"s1", ""} {
			t.Run(tt.name+" session "+session, func(t *testing.T) {
				a := tt.answer
				a.Reasoning, a.Via = " we already\x07 run it\n", model.AnsweredCLI
				d, err := Resolve(openDecision(tt.mode, session), a, now)
				if err != nil {
					t.Fatalf("Resolve: %v", err)
				}
				want := openDecision(tt.mode, session)
				want.Status, want.Delivery = tt.status, tt.delivery
				if session == "" {
					want.Delivery = model.DeliveryNone
				}
				want.AnswerKey, want.Answer, want.Scope = tt.answerKey, tt.answerText, tt.scope
				want.AnswerReasoning, want.AnsweredVia, want.AnsweredAt = "we already run it", model.AnsweredCLI, now
				if !reflect.DeepEqual(d, want) {
					t.Errorf("Resolve =\n%+v\nwant\n%+v", d, want)
				}
			})
		}
	}
}

func TestResolveRejectsBadAnswers(t *testing.T) {
	bad := map[string]model.Answer{
		"both key and text":             {Key: "B", Text: "Redis"},
		"neither":                       {},
		"key b when option is B":        {Key: "b"},
		"unknown key":                   {Key: "Z"},
		"501-rune text":                 {Text: strings.Repeat("x", 501)},
		"text empty after clean":        {Text: " \x1b[2J\n "},
		"1001-rune reasoning":           {Key: "A", Reasoning: strings.Repeat("r", 1001)},
		"scope x":                       {Key: "A", Scope: "x"},
		"newline + 501-rune text":       {Text: "\n" + strings.Repeat("x", MaxAnswerText+1)},
		"newline + 900-rune text":       {Text: "\n" + strings.Repeat("x", 900)},
		"newline + 1001-rune reasoning": {Key: "A", Reasoning: "\n" + strings.Repeat("r", MaxReasoning+1)},
	}
	for name, a := range bad {
		if _, err := Resolve(openDecision(model.ModeBlocking, "s1"), a, time.Unix(0, 0)); !errors.Is(err, ErrBadAnswer) {
			t.Errorf("%s: err = %v, want ErrBadAnswer", name, err)
		}
	}
	ok := model.Answer{Text: "\n" + strings.Repeat("x", MaxAnswerText), Reasoning: "\n\n" + strings.Repeat("r", MaxReasoning)}
	d, err := Resolve(openDecision(model.ModeBlocking, "s1"), ok, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("answer at the limits: %v", err)
	}
	if d.Answer != strings.Repeat("x", MaxAnswerText) || d.AnswerReasoning != strings.Repeat("r", MaxReasoning) {
		t.Errorf("answer at the limits stored as %.20q / %.20q", d.Answer, d.AnswerReasoning)
	}
}

func TestIsDecided(t *testing.T) {
	for _, s := range []model.DecisionStatus{model.StatusAnswered, model.StatusConfirmed, model.StatusOverridden} {
		if !IsDecided(s) {
			t.Errorf("IsDecided(%q) = false", s)
		}
	}
	for _, s := range []model.DecisionStatus{model.StatusOpen, model.StatusWithdrawn, model.StatusExpired, ""} {
		if IsDecided(s) {
			t.Errorf("IsDecided(%q) = true", s)
		}
	}
}
