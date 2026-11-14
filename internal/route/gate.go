// Package route holds the pure routing rules of plan section 4.10: the
// targets a message can go to, how an address names them, what a pick
// means for each kind, the router model's contract and the auto-send gate.
package route

import (
	"github.com/ZachFara/curia/internal/model"
)

// Gate and auto-send constants (plan section 4.10).
const (
	Window      = 50   // labels the gate looks at
	MinAccepted = 48   // 95% of Window, rounded up
	CloseCall   = 0.15 // the least lead over the runner-up that is not a close call
	SampleEvery = 10   // every tenth eligible message asks anyway
)

// GateResult is the accuracy over the newest labels.
type GateResult struct {
	Labeled, Accepted int // of at most Window
	Earned            bool
}

// Gate computes the accuracy gate over labels at or above the threshold,
// newest first. Only the newest Window count.
func Gate(labels []model.RouteLabel) GateResult {
	if len(labels) > Window {
		labels = labels[:Window]
	}
	g := GateResult{Labeled: len(labels)}
	for _, l := range labels {
		if l.Accepted {
			g.Accepted++
		}
	}
	g.Earned = g.Labeled == Window && g.Accepted >= MinAccepted
	return g
}

// AutoInput is everything the auto-send rule looks at.
type AutoInput struct {
	Configured, Tripped bool
	Threshold           float64
	Gate                GateResult
	Suggestion          Suggestion
	TargetAlive         bool
	EligibleSoFar       int // earlier auto_eligible messages
}

// Auto reports whether to send without asking, whether the message counts
// as auto-eligible, and why not.
func Auto(in AutoInput) (send, eligible bool, why string) {
	s := in.Suggestion
	switch {
	case !in.Configured:
		return false, false, "auto-send is not configured"
	case in.Tripped:
		return false, false, "auto-send is tripped"
	case !in.Gate.Earned:
		return false, false, "auto-send is not earned"
	case s.Confidence < in.Threshold:
		return false, false, "confidence is below the threshold"
	case s.RunnerUp.Key != "" && (!s.HasRunnerUpConfidence || s.Confidence-s.RunnerUpConfidence <= CloseCall):
		return false, false, "close call"
	case s.Kind == model.KindRule:
		return false, false, "a rule is always confirmed"
	case s.Kind == model.KindInstruction && !in.TargetAlive:
		return false, false, "the target session is not alive"
	}
	if in.EligibleSoFar%SampleEvery == 0 {
		return false, true, "sampled"
	}
	return true, true, ""
}

// Tripped reports whether a new label trips auto-send off: it was earned
// and the newest Window labels now hold fewer than MinAccepted accepted.
func Tripped(earned bool, g GateResult) bool {
	return earned && g.Labeled >= Window && g.Accepted < MinAccepted
}
