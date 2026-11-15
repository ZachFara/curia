package route

import (
	"strings"
	"testing"

	"github.com/ZachFara/curia/internal/model"
)

func labels(n, accepted int) []model.RouteLabel {
	out := make([]model.RouteLabel, n)
	for i := range out {
		out[i] = model.RouteLabel{Confidence: 0.95, Accepted: i < accepted}
	}
	return out
}

func TestGateNeedsFiftyLabels(t *testing.T) {
	if g := Gate(labels(49, 49)); g.Earned || g.Labeled != 49 {
		t.Errorf("49: %+v", g)
	}
	if g := Gate(labels(50, 48)); !g.Earned || g.Accepted != 48 {
		t.Errorf("48/50: %+v", g)
	}
	if g := Gate(labels(50, 47)); g.Earned {
		t.Errorf("47/50: %+v", g)
	}
	if g := Gate(labels(80, 80)); g.Labeled != 50 || g.Accepted != 50 {
		t.Errorf("window: %+v", g)
	}
}

func goodInput() AutoInput {
	return AutoInput{
		Configured: true, Threshold: 0.9, Gate: GateResult{Labeled: 50, Accepted: 50, Earned: true},
		Suggestion:  Suggestion{Kind: model.KindInstruction, Confidence: 0.95},
		TargetAlive: true, EligibleSoFar: 3,
	}
}

func TestAutoTable(t *testing.T) {
	if send, el, why := Auto(goodInput()); !send || !el || why != "" {
		t.Fatalf("all hold: %v %v %q", send, el, why)
	}
	cases := []struct {
		name   string
		mut    func(*AutoInput)
		why    string
		eligOK bool
	}{
		{"configured", func(i *AutoInput) { i.Configured = false }, "not configured", false},
		{"tripped", func(i *AutoInput) { i.Tripped = true }, "tripped", false},
		{"earned", func(i *AutoInput) { i.Gate.Earned = false }, "not earned", false},
		{"confident", func(i *AutoInput) { i.Suggestion.Confidence = 0.89 }, "below the threshold", false},
		{"close", func(i *AutoInput) {
			i.Suggestion.RunnerUp = Candidate{Key: "s2"}
			i.Suggestion.HasRunnerUpConfidence = true
			i.Suggestion.RunnerUpConfidence = 0.9
		}, "close call", false},
		{"rule", func(i *AutoInput) { i.Suggestion.Kind = model.KindRule }, "rule", false},
		{"dead", func(i *AutoInput) { i.TargetAlive = false }, "not alive", false},
	}
	for _, tc := range cases {
		in := goodInput()
		tc.mut(&in)
		send, el, why := Auto(in)
		if send || el || !strings.Contains(why, tc.why) {
			t.Errorf("%s: %v %v %q", tc.name, send, el, why)
		}
	}
	in := goodInput()
	in.Suggestion.Kind = model.KindTask
	in.TargetAlive = false
	if send, _, _ := Auto(in); !send {
		t.Error("a task does not need a live session")
	}
}

func TestAutoCloseCallAndMissingConfidence(t *testing.T) {
	in := goodInput()
	in.Suggestion.Confidence = 0.95
	in.Suggestion.RunnerUp = Candidate{Key: "s2"}
	in.Suggestion.HasRunnerUpConfidence = true
	in.Suggestion.RunnerUpConfidence = 0.81
	if send, _, why := Auto(in); send || why != "close call" {
		t.Errorf("0.81: %v %q", send, why)
	}
	in.Suggestion.RunnerUpConfidence = 0.79
	if send, _, _ := Auto(in); !send {
		t.Error("0.79 should send")
	}
	in.Suggestion.HasRunnerUpConfidence = false
	if send, _, why := Auto(in); send || why != "close call" {
		t.Errorf("missing: %v %q", send, why)
	}
}

func TestSamplingEveryTenth(t *testing.T) {
	for _, n := range []int{0, 10, 20} {
		in := goodInput()
		in.EligibleSoFar = n
		if send, el, why := Auto(in); send || !el || why != "sampled" {
			t.Errorf("%d: %v %v %q", n, send, el, why)
		}
	}
	for n := 1; n <= 9; n++ {
		in := goodInput()
		in.EligibleSoFar = n
		if send, el, _ := Auto(in); !send || !el {
			t.Errorf("%d: %v %v", n, send, el)
		}
	}
}

func TestTripped(t *testing.T) {
	if Tripped(false, Gate(labels(50, 10))) {
		t.Error("never earned")
	}
	if !Tripped(true, Gate(labels(50, 47))) {
		t.Error("47 of 50")
	}
	if Tripped(true, Gate(labels(49, 0))) {
		t.Error("49 labels")
	}
	if Tripped(true, Gate(labels(50, 48))) {
		t.Error("still holds")
	}
}
