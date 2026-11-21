package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/ui"
)

// The inbox's rows are the badge's counts: D, W and R, asserted against the
// badge's own function, and the open assume tickets (which the badge does
// not show) against the store's count.
func TestInboxCountsEqualTheBadge(t *testing.T) {
	db := decisionsHome(t, decisionsSpecs()...) // D-1 blocking and D-2 assume are open
	seedSessions(t, db,
		model.Session{ID: "w1", Agent: "claude", State: model.StateWaiting, Pane: "%1", UpdatedAt: at(11, 0), LastUpdate: "Allow Bash?"},
		model.Session{ID: "w2", Agent: "claude", State: model.StateWaiting, Pane: "%2", UpdatedAt: at(10, 0)},
		model.Session{ID: "w3", Agent: "claude", State: model.StateWorking, Pane: "%3", UpdatedAt: at(10, 0)},
	)
	uiReviewStore(t, db) // one pending review, one of a dropped task
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	items, err := inboxSource{st: st}.Items(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var n [4]int
	var order []string
	for _, it := range items {
		n[it.Kind]++
		order = append(order, it.Label)
	}
	badge, err := statusBadge(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("D%d W%d R%d", n[ui.InboxDecision], n[ui.InboxPrompt], n[ui.InboxReview]); badge != want {
		t.Errorf("badge = %q, inbox counts say %q", badge, want)
	}
	counts, err := st.OpenCounts(ctx, clock())
	if err != nil {
		t.Fatal(err)
	}
	if got := counts["s1"].Assume; got != n[ui.InboxAssume] {
		t.Errorf("assume items = %d, open assume tickets = %d", n[ui.InboxAssume], got)
	}
	// Groups in order, oldest first within one: w2 waits longer than w1.
	if got, want := strings.Join(order, " "), "s1 w2 w1 T-1 s1"; got != want {
		t.Errorf("order = %q, want %q", got, want)
	}
	if items[1].Session.Pane != "%2" || items[2].Summary != "Allow Bash?" {
		t.Errorf("prompt items = %+v, %+v", items[1], items[2])
	}
}

// inboxCmdSeed is the store of TestInboxCountsEqualTheBadge, with an
// untrusted summary on one prompt.
func inboxCmdSeed(t *testing.T) {
	t.Helper()
	db := decisionsHome(t, decisionsSpecs()...)
	seedSessions(t, db,
		model.Session{ID: "w1", Agent: "claude", State: model.StateWaiting, Pane: "%1", UpdatedAt: at(11, 0), LastUpdate: "Allow \x1b[31mBash?\x1b[0m\n" + strings.Repeat("x", 500)},
		model.Session{ID: "w2", Agent: "claude", State: model.StateWaiting, Pane: "%2", UpdatedAt: at(10, 0)},
	)
	uiReviewStore(t, db)
}

func TestInboxCommandJSONCountsEqualTheBadge(t *testing.T) {
	inboxCmdSeed(t)
	defer func() { inboxJSON = false }()
	out, err := runRoot(t, "inbox", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Counts map[string]int `json:"counts"`
		Items  []struct {
			Kind, ID, Session, Task, Summary string
			Since                            string
		}
	}
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&got); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	badge, err := statusBadge(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	c := got.Counts
	if want := fmt.Sprintf("D%d W%d R%d", c["decisions"], c["prompts"], c["reviews"]); badge != want || c["assumptions"] != 1 {
		t.Errorf("badge %q, counts %v", badge, c)
	}
	var kinds []string
	for _, it := range got.Items {
		kinds = append(kinds, it.Kind)
		if len(it.Summary) > 200 || strings.ContainsAny(it.Summary, "\x1b\n") {
			t.Errorf("summary not cleaned and bounded: %q", it.Summary)
		}
	}
	if k := strings.Join(kinds, " "); k != "decision prompt prompt review assume" {
		t.Errorf("kinds = %q", k)
	}
	if got.Items[1].ID != "w2" || got.Items[1].Since == "" {
		t.Errorf("prompt item = %+v", got.Items[1])
	}
}

func TestInboxCommandTextAndEmpty(t *testing.T) {
	inboxCmdSeed(t)
	out, err := runRoot(t, "inbox")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "w2") || !strings.Contains(out, "Allow Bash?") || strings.Contains(out, "\x1b") {
		t.Errorf("text:\n%q", out)
	}
	decisionsHome(t) // an empty store
	out, _ = runRoot(t, "inbox")
	if strings.TrimSpace(out) != "nothing needs you" {
		t.Errorf("empty text = %q", out)
	}
	defer func() { inboxJSON = false }()
	out, _ = runRoot(t, "inbox", "--json")
	if !strings.Contains(out, `"items": []`) {
		t.Errorf("empty json = %q", out)
	}
}
