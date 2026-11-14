package route

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/task"
)

// capturedCands are the targets of the probe's prompt: a session s1, the
// task T-42, a repo r1 and global.
func capturedCands() []Candidate {
	s := []model.Session{{ID: "a1", Repo: "/w/api-server", Branch: "main", State: model.StateWorking}}
	c := []task.Card{{Task: model.Task{ID: "T-42", Title: "auth refactor", Repo: "/w/api-server"}, Status: model.TaskReady}}
	return Candidates(s, c)
}

func wrap(t *testing.T, result string, isErr bool) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": isErr, "result": result})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseSuggestionAcceptsCaptured(t *testing.T) {
	want := []struct {
		target, kind, runner string
		conf, rconf          float64
	}{
		{"s1", "instruction", "T-42", 0.7, 0.2},
		{"global", "rule", "r1", 0.93, 0.05},
		{"r1", "task", "s1", 0.9, 0.08},
	}
	for i, w := range want {
		raw, err := os.ReadFile(filepath.Join("testdata", "envelope-main-case"+string(rune('0'+i))+".json"))
		if err != nil {
			t.Fatal(err)
		}
		got, err := ParseSuggestion(raw, capturedCands())
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if got.Target.Key != w.target || string(got.Kind) != w.kind || got.RunnerUp.Key != w.runner ||
			got.Confidence != w.conf || !got.HasRunnerUpConfidence || got.RunnerUpConfidence != w.rconf {
			t.Errorf("case %d: %+v", i, got)
		}
	}
}

func TestParseSuggestionRejects(t *testing.T) {
	ok := `{"target":"s1","kind":"instruction","confidence":0.9,"runner_up":"","runner_up_confidence":0}`
	if _, err := ParseSuggestion(wrap(t, ok, false), capturedCands()); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	cases := map[string][]byte{
		"unoffered key":    wrap(t, strings.Replace(ok, `"s1"`, `"s9"`, 1), false),
		"bad kind":         wrap(t, strings.Replace(ok, `"instruction"`, `"delete"`, 1), false),
		"conf high":        wrap(t, strings.Replace(ok, `0.9,`, `1.5,`, 1), false),
		"conf negative":    wrap(t, strings.Replace(ok, `0.9,`, `-0.1,`, 1), false),
		"conf string":      wrap(t, strings.Replace(ok, `0.9,`, `"high",`, 1), false),
		"runner is target": wrap(t, strings.Replace(ok, `"runner_up":""`, `"runner_up":"s1"`, 1), false),
		"runner unoffered": wrap(t, strings.Replace(ok, `"runner_up":""`, `"runner_up":"zz"`, 1), false),
		"runner conf high": wrap(t, strings.Replace(ok, `"runner_up_confidence":0`, `"runner_up_confidence":2`, 1), false),
		"prose only":       wrap(t, "I think it goes to s1", false),
		"prose before":     wrap(t, "Sure: "+ok, false),
		"prose after":      wrap(t, ok+" hope that helps", false),
		"two objects":      wrap(t, ok+"\n"+ok, false),
		"unknown field":    wrap(t, strings.Replace(ok, `"kind"`, `"extra":1,"kind"`, 1), false),
		"missing kind":     wrap(t, `{"target":"s1","confidence":0.9,"runner_up":""}`, false),
		"missing target":   wrap(t, `{"kind":"task","confidence":0.9,"runner_up":""}`, false),
		"missing conf":     wrap(t, `{"target":"s1","kind":"task","runner_up":""}`, false),
		"missing runner":   wrap(t, `{"target":"s1","kind":"task","confidence":0.9}`, false),
		"null target":      wrap(t, `{"target":null,"kind":"task","confidence":0.9,"runner_up":""}`, false),
		"is_error":         wrap(t, ok, true),
		"not an envelope":  []byte(ok),
		"no result":        []byte(`{"type":"result","is_error":false}`),
		"empty":            nil,
		"too big":          []byte(strings.Repeat(" ", 65<<10)),
		"two fences":       wrap(t, "```json\n"+ok+"\n```\n```json\n"+ok+"\n```", false),
	}
	for name, in := range cases {
		if _, err := ParseSuggestion(in, capturedCands()); !errors.Is(err, ErrNoSuggestion) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestParseSuggestionOptionalRunnerConfidence(t *testing.T) {
	r := wrap(t, `{"target":"s1","kind":"task","confidence":0.9,"runner_up":"r1"}`, false)
	got, err := ParseSuggestion(r, capturedCands())
	if err != nil || got.HasRunnerUpConfidence || got.RunnerUp.Key != "r1" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestParseSuggestionFindsObjectInFence(t *testing.T) {
	obj := `{"target":"r1","kind":"task","confidence":0.8,"runner_up":"","runner_up_confidence":0}`
	got, err := ParseSuggestion(wrap(t, "```json\n"+obj+"\n```", false), capturedCands())
	if err != nil || got.Target.Key != "r1" {
		t.Fatalf("%+v %v", got, err)
	}
}
