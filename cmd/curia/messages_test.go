package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

const messagesNow = "2026-11-11T12:00:00Z"

// messagesSpec is one seeded message: created by CreateMessage, then moved
// to its status by a raw UPDATE so every value is exact.
type messagesSpec struct {
	text, status, via, ref string
	kind                   model.MessageKind
	session                string
	confirmed              bool
	hold                   time.Time
	conf                   *float64
}

func messagesHome(t *testing.T, specs ...messagesSpec) string {
	t.Helper()
	db := hookHome(t)
	now, err := time.Parse(time.RFC3339, messagesNow)
	if err != nil {
		t.Fatal(err)
	}
	prev := clock
	clock = func() time.Time { return now }
	t.Cleanup(func() {
		clock = prev
		messagesStatus, messagesJSON = "open", false
	})
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	for i, sp := range specs {
		m := model.Message{
			Text: sp.text, Kind: sp.kind, Sender: model.SenderUser, TargetSession: sp.session,
			RoutedBy: model.RoutedAddress, Status: model.MsgQueued, CreatedAt: now.Add(-time.Hour),
		}
		if m.Kind == "" {
			m.Kind = model.KindInstruction
		}
		if sp.conf != nil {
			m.Confidence, m.HasConfidence = *sp.conf, true
		}
		if sp.status == "held" {
			m.Status, m.HoldUntil = model.MsgHeld, sp.hold
		}
		stored, err := st.CreateMessage(context.Background(), m)
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		switch sp.status {
		case "", "queued", "held":
			continue
		}
		var confirmed any
		if sp.confirmed {
			confirmed = "2026-11-11T11:30:00.000000000Z"
		}
		if _, err := raw.Exec(`UPDATE messages SET status = ?, delivered_via = ?, result_ref = ?, delivered_at = ?, confirmed_at = ? WHERE id = ?`,
			sp.status, sp.via, sp.ref, "2026-11-11T11:00:00.000000000Z", confirmed, stored.ID); err != nil {
			t.Fatalf("seed %s: %v", stored.ID, err)
		}
	}
	return db
}

func messagesIDs(t *testing.T) []string {
	t.Helper()
	out, err := runRoot(t, "messages", "ls", "--json")
	if err != nil {
		t.Fatalf("messages ls: %v\n%s", err, out)
	}
	var rows []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	ids := []string{}
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

func messagesOneOfEach() []messagesSpec {
	fresh := time.Date(2026, 11, 11, 11, 59, 58, 0, time.UTC)
	return []messagesSpec{
		{text: "held one", status: "held", hold: fresh},                                 // M-1
		{text: "queued one", status: "queued"},                                          // M-2
		{text: "typed one", status: "delivered", via: "send_keys"},                      // M-3
		{text: "confirmed one", status: "delivered", via: "send_keys", confirmed: true}, // M-4
		{text: "hooked one", status: "delivered", via: "stop_hook"},                     // M-5
		{text: "undone one", status: "undone"},                                          // M-6
		{text: "canceled one", status: "canceled"},                                      // M-7
	}
}

func TestMessagesLsDefaultIsOpen(t *testing.T) {
	messagesHome(t, messagesOneOfEach()...)
	if got := strings.Join(messagesIDs(t), " "); got != "M-1 M-2 M-3" {
		t.Errorf("open = %q, want M-1 M-2 M-3", got)
	}
	messagesStatus = "all"
	if got := len(messagesIDs(t)); got != 7 {
		t.Errorf("all = %d, want 7", got)
	}
	messagesStatus = "delivered"
	if got := strings.Join(messagesIDs(t), " "); got != "M-3 M-4 M-5" {
		t.Errorf("delivered = %q", got)
	}
	messagesStatus = "bogus"
	if _, err := runRoot(t, "messages", "ls"); err == nil {
		t.Error("bogus status accepted")
	}
}

func messagesWantKeys(t *testing.T, row map[string]any, keys []string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := row[k]; !ok {
			t.Errorf("missing key %s", k)
		}
	}
}

func TestMessagesLsJSON(t *testing.T) {
	conf := 0.93
	messagesHome(t,
		messagesSpec{text: "plain", status: "queued", session: "s1"},
		messagesSpec{text: "rule", status: "delivered", via: "applied", ref: "D-40", kind: model.KindRule, conf: &conf},
	)
	messagesStatus = "all"
	out, err := runRoot(t, "messages", "ls", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	keys := []string{
		"id", "text", "kind", "sender", "sender_session", "target_session", "target_task", "target_repo",
		"suggested_target", "suggested_kind", "runner_up", "confidence", "routed_by", "auto_eligible", "status",
		"delivered_via", "result_ref", "created_at", "hold_until", "delivered_at", "confirmed_at",
	}
	if len(rows) != 2 || len(rows[0]) != len(keys) {
		t.Fatalf("rows = %v", rows)
	}
	messagesWantKeys(t, rows[0], keys)
	a, b := rows[0], rows[1]
	if a["confidence"] != nil || a["hold_until"] != nil || a["delivered_at"] != nil || a["confirmed_at"] != nil {
		t.Errorf("unset values must be null: %v", a)
	}
	if a["created_at"] != "2026-11-11T11:00:00Z" {
		t.Errorf("created_at = %v", a["created_at"])
	}
	if b["confidence"] != 0.93 || b["result_ref"] != "D-40" || b["delivered_via"] != "applied" || b["delivered_at"] == nil || b["confirmed_at"] != nil {
		t.Errorf("second row = %v", b)
	}
}

func TestMessagesLsEmptyIsEmptyArray(t *testing.T) {
	messagesHome(t, messagesSpec{text: "a", status: "queued"})
	messagesStatus = "canceled"
	if out, err := runRoot(t, "messages", "ls", "--json"); err != nil || strings.TrimSpace(out) != "[]" {
		t.Errorf("empty = %q, %v", out, err)
	}
}

func TestMessagesLsMarksUncollectable(t *testing.T) {
	for state, mark := range map[model.State]string{
		model.StateEnded: "queued (session ended)",
		model.StateStale: "queued (session stale)",
		"":               "queued (no session row)",
		model.StateIdle:  "queued",
	} {
		db := messagesHome(t, messagesSpec{text: "hi", status: "queued", session: "s1"})
		if state != "" {
			seedSessions(t, db, statusSession(state))
		}
		out, err := runRoot(t, "messages", "ls")
		if err != nil {
			t.Fatal(err)
		}
		var row string
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "M-1 ") {
				row = strings.Join(strings.Fields(l), " ")
			}
		}
		if !strings.HasSuffix(row, mark+" hi") {
			t.Errorf("session %q: row = %q, want it to hold %q", state, row, mark)
		}
	}
}

func TestMessagesLsStatusWords(t *testing.T) {
	messagesHome(t, messagesOneOfEach()...)
	messagesStatus = "all"
	out, err := runRoot(t, "messages", "ls")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"typed, unconfirmed", "delivered (send_keys)", "delivered (stop_hook)", "undone", "canceled", "KIND  "} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
}

func TestMessagesLsTableIsSanitised(t *testing.T) {
	messagesHome(t, messagesSpec{text: "evil\x1b[31m red\x07\nline two\ttab", status: "queued", session: "s\x1b]0;x\x07"})
	out, err := runRoot(t, "messages", "ls")
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out, "\x1b\x07") || strings.Count(out, "\n") != 2 {
		t.Errorf("control characters or extra lines in %q", out)
	}
	jout, err := runRoot(t, "messages", "ls", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(jout, "\x1b\x07") {
		t.Errorf("raw control character in JSON: %q", jout)
	}
}

func TestMessagesLsSweepsHeld(t *testing.T) {
	old := time.Date(2026, 11, 11, 11, 59, 0, 0, time.UTC) // 60 s past
	messagesHome(t,
		messagesSpec{text: "dead say", status: "held", hold: old},
		messagesSpec{text: "live say", status: "held", hold: time.Date(2026, 11, 11, 11, 59, 50, 0, time.UTC)},
	)
	if got := strings.Join(messagesIDs(t), " "); got != "M-2" {
		t.Errorf("open = %q, want only the live held M-2", got)
	}
	messagesStatus = "canceled"
	if got := strings.Join(messagesIDs(t), " "); got != "M-1" {
		t.Errorf("canceled = %q, want M-1", got)
	}
}

func TestMessagesCancel(t *testing.T) {
	db := messagesHome(t,
		messagesSpec{text: "a", status: "queued"},
		messagesSpec{text: "b", status: "delivered", via: "stop_hook"},
		messagesSpec{text: "c", status: "held", hold: time.Date(2026, 11, 11, 11, 59, 58, 0, time.UTC)},
	)
	out, err := runRoot(t, "messages", "cancel", "M-1")
	if err != nil || !strings.Contains(out, "M-1 canceled") {
		t.Fatalf("cancel M-1 = %q, %v", out, err)
	}
	for id, want := range map[string]string{
		"M-2":  "M-2 is delivered (stop_hook)",
		"M-1":  "M-1 is canceled",
		"M-3":  "M-3 is held",
		"M-99": "no such message",
		"x":    "invalid message id",
	} {
		out, cerr := runRoot(t, "messages", "cancel", id)
		if cerr == nil || !strings.Contains(cerr.Error(), want) {
			t.Errorf("cancel %s = %q, %v; want error with %q", id, out, cerr, want)
		}
		if strings.HasPrefix(out, id+" canceled") {
			t.Errorf("cancel %s claimed a cancel: %q", id, out)
		}
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if m, _, _ := st.GetMessage(context.Background(), "M-2"); m.Status != model.MsgDelivered {
		t.Errorf("M-2 = %s", m.Status)
	}
}

func TestMessagesCancelRefusesInsideAgent(t *testing.T) {
	db := messagesHome(t, messagesSpec{text: "a", status: "queued"})
	t.Setenv("CLAUDECODE", "1")
	_, err := runRoot(t, "messages", "cancel", "M-1")
	if err == nil || !strings.Contains(err.Error(), "refusing to run inside an agent session") {
		t.Fatalf("err = %v", err)
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if m, _, _ := st.GetMessage(context.Background(), "M-1"); m.Status != model.MsgQueued {
		t.Errorf("status = %s, want queued", m.Status)
	}
}

func TestMessagesCancelRefusesBeforeTheStore(t *testing.T) {
	db := hookHome(t)
	t.Setenv("CLAUDECODE", "1")
	if _, err := runRoot(t, "messages", "cancel", "M-1"); err == nil {
		t.Fatal("no refusal")
	}
	if _, statErr := os.Stat(db); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("touched the database: %v", statErr)
	}
}
