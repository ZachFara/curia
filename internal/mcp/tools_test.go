package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

// toolsT0 is the fixed start time of every fixture.
var toolsT0 = time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)

// notified is one call of Deps.Notify.
type notified struct {
	d    model.Decision
	pane string
}

// fixture is the four tools on a real store in a temp dir, with a clock,
// caller and log the test controls.
type fixture struct {
	t        *testing.T
	st       *store.Store
	now      time.Time
	caller   Caller
	notified []notified
	log      bytes.Buffer
	tools    []Tool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "curia.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &fixture{t: t, st: st, now: toolsT0, caller: Caller{SessionID: "S1", Repo: "/r/a", Pane: "%3"}}
	f.tools = Tools(Deps{
		Store:  st,
		Cfg:    config.Default(),
		Now:    func() time.Time { return f.now },
		Caller: func(context.Context) Caller { return f.caller },
		Notify: func(_ context.Context, d model.Decision, pane string) {
			f.notified = append(f.notified, notified{d, pane})
		},
		Log: slog.New(slog.NewTextHandler(&f.log, nil)),
	})
	return f
}

// call runs tool name with args (a string is sent as is, anything else is
// encoded) and returns the result as the agent reads it: decoded from JSON.
func (f *fixture) call(name string, args any) (map[string]any, *ToolError) {
	f.t.Helper()
	raw, ok := args.(string)
	if !ok {
		b, err := json.Marshal(args)
		if err != nil {
			f.t.Fatal(err)
		}
		raw = string(b)
	}
	i := slices.IndexFunc(f.tools, func(t Tool) bool { return t.Name == name })
	if i < 0 {
		f.t.Fatalf("no tool %q", name)
	}
	res, terr := f.tools[i].Call(context.Background(), json.RawMessage(raw))
	if terr != nil {
		if res != nil {
			f.t.Errorf("%s returned a result with its error: %v", name, res)
		}
		return nil, terr
	}
	b, err := marshal(res)
	if err != nil {
		f.t.Fatalf("%s result does not encode: %v", name, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		f.t.Fatalf("%s result %s is not a JSON object: %v", name, b, err)
	}
	return m, nil
}

// mustCall is call that fails the test on a tool error.
func (f *fixture) mustCall(name string, args any) map[string]any {
	f.t.Helper()
	m, terr := f.call(name, args)
	if terr != nil {
		f.t.Fatalf("%s: unexpected tool error %+v", name, terr)
	}
	return m
}

// wantError calls name and requires a tool error with code.
func (f *fixture) wantError(name string, args any, code string) *ToolError {
	f.t.Helper()
	m, terr := f.call(name, args)
	if terr == nil {
		f.t.Fatalf("%s(%v) = %v, want tool error %s", name, args, m, code)
	}
	if terr.Code != code {
		f.t.Fatalf("%s(%v) error code = %q (%+v), want %q", name, args, terr.Code, terr, code)
	}
	return terr
}

// get loads decision id from the store.
func (f *fixture) get(id string) model.Decision {
	f.t.Helper()
	d, found, err := f.st.GetDecision(context.Background(), id)
	if err != nil || !found {
		f.t.Fatalf("GetDecision(%s) = found %v, err %v", id, found, err)
	}
	return d
}

// answer answers id with option key at the fixture's time.
func (f *fixture) answer(id, key, reasoning string) {
	f.t.Helper()
	if _, err := f.st.AnswerDecision(context.Background(), id,
		model.Answer{Key: key, Reasoning: reasoning, Via: model.AnsweredCLI}, f.now); err != nil {
		f.t.Fatalf("AnswerDecision(%s): %v", id, err)
	}
}

// as runs fn with the caller set to c, then restores the caller.
func (f *fixture) as(c Caller, fn func()) {
	prev := f.caller
	f.caller = c
	defer func() { f.caller = prev }()
	fn()
}

// ticket is a valid blocking request_decision argument object; edit
// changes it.
func ticket(question string, edit ...func(map[string]any)) map[string]any {
	m := map[string]any{
		"question": question,
		"options": []map[string]any{
			{"key": "A", "label": "Postgres", "tradeoffs": "already run"},
			{"key": "B", "label": "Redis"},
		},
		"recommendation": "A",
		"rationale":      "We already operate it.",
		"reversibility":  "costly",
		"mode":           "blocking",
	}
	for _, e := range edit {
		e(m)
	}
	return m
}

func assume(m map[string]any) {
	m["mode"] = "assume"
	m["reversibility"] = "cheap"
	m["assumption"] = "we go with Postgres"
}

func set(key string, v any) func(map[string]any) {
	return func(m map[string]any) { m[key] = v }
}

const webhookQ = "Store webhook events in Postgres or Redis?"

// decided raises question from caller c with edits and answers it with B.
func (f *fixture) decided(c Caller, question string, edit ...func(map[string]any)) {
	f.t.Helper()
	var id string
	f.as(c, func() { id = typed[string](f.t, f.mustCall("request_decision", ticket(question, edit...))["id"]) })
	f.answer(id, "B", "we already run Redis")
}

// typed asserts that v is a T, failing the test otherwise.
func typed[T any](t *testing.T, v any) T {
	t.Helper()
	x, ok := v.(T)
	if !ok {
		t.Fatalf("%v (%T) is not a %T", v, v, x)
	}
	return x
}

var otherInRepoA = Caller{SessionID: "S0", Repo: "/r/a", Pane: "%9"}

func precedentIDs(t *testing.T, m map[string]any) []string {
	t.Helper()
	raw, ok := m["precedent"].([]any)
	if !ok {
		t.Fatalf("precedent is not an array: %v", m["precedent"])
	}
	ids := []string{}
	for _, p := range raw {
		ids = append(ids, typed[string](t, typed[map[string]any](t, p)["id"]))
	}
	return ids
}

func TestToolSurfaceIsExactlyFour(t *testing.T) {
	f := newFixture(t)
	var names []string
	for _, tool := range f.tools {
		names = append(names, tool.Name)
		for _, bad := range []string{"answer", "confirm", "approve", "override", "edit"} {
			if strings.Contains(tool.Name, bad) {
				t.Errorf("tool %q contains %q", tool.Name, bad)
			}
		}
		var schema map[string]any
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Errorf("%s: input schema is not a JSON object: %v", tool.Name, err)
		} else if schema["type"] != "object" {
			t.Errorf("%s: schema type = %v, want object", tool.Name, schema["type"])
		}
		if tool.Call == nil {
			t.Errorf("%s has no Call", tool.Name)
		}
	}
	want := []string{"search_decisions", "request_decision", "check_decision", "withdraw_decision"}
	if !slices.Equal(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	descs := map[string]string{
		"search_decisions":  "Search past decisions the user already made. Call before raising a ticket.",
		"request_decision":  "Raise a decision ticket for the user: a question, 2 to 4 options, your recommendation and why.",
		"check_decision":    "Read a ticket's status and, once decided, the answer.",
		"withdraw_decision": "Withdraw your own open ticket, giving the reason or the past decision that settles it.",
	}
	for _, tool := range f.tools {
		if tool.Description != descs[tool.Name] {
			t.Errorf("%s description = %q, want %q", tool.Name, tool.Description, descs[tool.Name])
		}
	}
}

// TestUnknownToolNameIsAnError: through the server, a tool that answers,
// approves or edits does not exist.
func TestUnknownToolNameIsAnError(t *testing.T) {
	f := newFixture(t)
	h := newHarness(t, f.tools...)
	for i, name := range []string{"answer_decision", "confirm_decision", "approve", "edit_decision", "Request_Decision"} {
		h.send(`{"jsonrpc":"2.0","id":` + string(rune('1'+i)) + `,"method":"tools/call","params":{"name":"` + name + `","arguments":{"id":"D-1"}}}`)
		if code := errCode(t, h.recv()); code != codeInvalidParams {
			t.Errorf("tools/call %s: code %v, want %d", name, code, codeInvalidParams)
		}
	}
	h.finish()
}

func TestSchemasAreExact(t *testing.T) {
	f := newFixture(t)
	want := map[string]string{
		"search_decisions": `{"type":"object","required":["query"],"properties":{
  "query":{"type":"string","maxLength":500,"description":"The question in plain words."},
  "repo":{"type":"string","description":"Repository path to match repo-scoped decisions against. Default: your session's repository."},
  "tags":{"type":"array","maxItems":8,"items":{"type":"string"},"description":"Return only decisions carrying at least one of these tags."}}}`,
		"check_decision": `{"type":"object","required":["id"],"properties":{"id":{"type":"string","pattern":"^D-[1-9][0-9]{0,8}$"}}}`,
		"withdraw_decision": `{"type":"object","required":["id","reason"],"properties":{
  "id":{"type":"string","pattern":"^D-[1-9][0-9]{0,8}$"},
  "reason":{"type":"string","maxLength":300},
  "precedent_id":{"type":"string","description":"The past decision that settles this, if one does."}}}`,
	}
	for _, tool := range f.tools {
		w, ok := want[tool.Name]
		if !ok {
			continue
		}
		var got, exp any
		if err := json.Unmarshal(tool.InputSchema, &got); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(w), &exp); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, exp) {
			t.Errorf("%s schema = %s", tool.Name, tool.InputSchema)
		}
	}
	var req map[string]any
	if err := json.Unmarshal(f.tools[1].InputSchema, &req); err != nil {
		t.Fatal(err)
	}
	props := typed[map[string]any](t, req["properties"])
	var keys []string
	for k := range props {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	wantKeys := []string{"assumption", "context", "differs_from", "differs_why", "mode", "options", "rationale", "recommendation", "reversibility", "scope", "tags", "question"}
	slices.Sort(wantKeys)
	if !slices.Equal(keys, wantKeys) {
		t.Errorf("request_decision properties = %v, want %v", keys, wantKeys)
	}
}

func TestRequestCreatesTicket(t *testing.T) {
	f := newFixture(t)
	m := f.mustCall("request_decision", ticket(webhookQ))
	want := map[string]any{
		"id": "D-1", "status": "open", "mode": "blocking", "duplicate": false, "precedent": []any{},
		"instruction": "Finish any safe work, then end your turn. The answer will arrive as a message; if it has not when you are next active, call check_decision.",
	}
	if !reflect.DeepEqual(m, want) {
		t.Errorf("result = %v, want %v", m, want)
	}
	d := f.get("D-1")
	if d.SessionID != "S1" || d.Repo != "/r/a" || d.Status != model.StatusOpen || d.Question != webhookQ {
		t.Errorf("row = session %q repo %q status %q question %q", d.SessionID, d.Repo, d.Status, d.Question)
	}

	m = f.mustCall("request_decision", ticket("Which queue for retries?", assume))
	if m["instruction"] != "Proceed with your assumption. You may be overridden later; if so, revert the work built on it." {
		t.Errorf("assume instruction = %q", m["instruction"])
	}
	if d := f.get("D-2"); !d.ExpiresAt.Equal(toolsT0.Add(24 * time.Hour)) {
		t.Errorf("assume ticket expires at %v, want %v", d.ExpiresAt, toolsT0.Add(24*time.Hour))
	}
}

func TestRequestCleansText(t *testing.T) {
	f := newFixture(t)
	f.mustCall("request_decision", ticket("  Use \x1b[31mPostgres\x1b[0m\nhere?  ", set("context", "line\x1b]52;c;aGk=\x07 one\r\n\r\n\r\ntwo")))
	d := f.get("D-1")
	if d.Question != "Use Postgres here?" || d.Context != "line one\n\ntwo" {
		t.Errorf("stored question %q context %q", d.Question, d.Context)
	}
}

func TestRequestInvalidListsAllProblems(t *testing.T) {
	f := newFixture(t)
	terr := f.wantError("request_decision", ticket("",
		set("options", []map[string]any{{"key": "A", "label": "Postgres"}}),
		set("mode", "sometimes")), "invalid_ticket")
	var fields []string
	for _, p := range terr.Problems {
		fields = append(fields, p.Field)
	}
	if want := []string{"question", "options", "mode"}; !slices.Equal(fields, want) {
		t.Errorf("problem fields = %v (%+v), want %v", fields, terr.Problems, want)
	}
	if ds, _ := f.st.ListDecisions(context.Background(), store.DecisionFilter{}); len(ds) != 0 {
		t.Errorf("rows = %d, want none", len(ds))
	}

	terr = f.wantError("request_decision", ticket(webhookQ, set("options", "A or B")), "invalid_ticket")
	if len(terr.Problems) != 1 || terr.Problems[0].Field != "options" {
		t.Errorf("type error problems = %+v, want one on options", terr.Problems)
	}
	// Validation runs on normalized input: a line break in the question is
	// squeezed, not reported, and a too-long field still fails.
	terr = f.wantError("request_decision", ticket("\n"+strings.Repeat("q", 301)), "invalid_ticket")
	if len(terr.Problems) != 1 || terr.Problems[0].Field != "question" {
		t.Errorf("long question problems = %+v", terr.Problems)
	}
}

func TestRequestIdenticalRetry(t *testing.T) {
	f := newFixture(t)
	f.mustCall("request_decision", ticket(webhookQ))
	m := f.mustCall("request_decision", ticket(webhookQ))
	if m["id"] != "D-1" || m["duplicate"] != true || m["status"] != "open" {
		t.Errorf("retry = %v, want D-1 duplicate open", m)
	}
	if m["instruction"] != "This ticket is already open. Do not raise it again." {
		t.Errorf("instruction = %q", m["instruction"])
	}
	if _, ok := m["answer_key"]; ok {
		t.Errorf("open duplicate carries answer fields: %v", m)
	}
	if ds, _ := f.st.ListDecisions(context.Background(), store.DecisionFilter{}); len(ds) != 1 {
		t.Errorf("rows = %d, want 1", len(ds))
	}
}

func TestRequestRetryAfterAnswerReturnsAnswer(t *testing.T) {
	f := newFixture(t)
	f.mustCall("request_decision", ticket(webhookQ))
	f.answer("D-1", "B", "we already run Redis")
	m := f.mustCall("request_decision", ticket(webhookQ))
	want := map[string]any{
		"id": "D-1", "status": "answered", "mode": "blocking", "duplicate": true, "precedent": []any{},
		"instruction": "Decided. Follow the answer.", "answer_key": "B", "answer": "B: Redis", "reasoning": "we already run Redis",
	}
	if !reflect.DeepEqual(m, want) {
		t.Errorf("retry after answer = %v, want %v", m, want)
	}

	// An overridden assume ticket says to revert.
	f.mustCall("request_decision", ticket("Which queue for retries?", assume))
	f.answer("D-2", "B", "")
	m = f.mustCall("request_decision", ticket("Which queue for retries?", assume))
	if m["status"] != "overridden" || m["answer_key"] != "B" || m["reasoning"] != "" ||
		m["instruction"] != "Overridden. Revert work based on your assumption, then follow the answer." {
		t.Errorf("overridden retry = %v", m)
	}
}

func TestRequestAfterSupersessionOpensNewTicket(t *testing.T) {
	f := newFixture(t)
	f.mustCall("request_decision", ticket(webhookQ))
	f.answer("D-1", "B", "")
	if _, err := f.st.EditDecision(context.Background(), "D-1", model.DecisionEdit{Supersede: true}, f.now); err != nil {
		t.Fatal(err)
	}
	m := f.mustCall("request_decision", ticket(webhookQ))
	if m["id"] != "D-2" || m["duplicate"] != false || len(precedentIDs(t, m)) != 0 {
		t.Errorf("after supersession = %v, want a new ticket D-2 with no precedent", m)
	}
}

func TestRequestReturnsStrongPrecedent(t *testing.T) {
	f := newFixture(t)
	f.decided(otherInRepoA, webhookQ)
	m := f.mustCall("request_decision", ticket("Use Postgres or Redis to store the webhook events?"))
	if m["id"] != "D-2" || m["status"] != "open" || m["duplicate"] != false {
		t.Errorf("new ticket = %v", m)
	}
	ps := typed[[]any](t, m["precedent"])
	if len(ps) != 1 {
		t.Fatalf("precedent = %v, want D-1", m["precedent"])
	}
	answered := f.get("D-1").AnsweredAt.UTC().Format(time.RFC3339)
	want := map[string]any{
		"id": "D-1", "question": webhookQ, "answer": "B: Redis", "reasoning": "we already run Redis",
		"scope": "repo", "repo": "a", "tags": []any{}, "answered_at": answered,
	}
	if !reflect.DeepEqual(ps[0], want) {
		t.Errorf("precedent[0] = %v, want %v", ps[0], want)
	}
	if m["instruction"] != "A past decision looks like this one. If it settles your question, call withdraw_decision with precedent_id. If your case differs, call request_decision again with the same ticket plus differs_from and differs_why. Until then the ticket stays open." {
		t.Errorf("instruction = %q", m["instruction"])
	}
	if d := f.get("D-2"); d.Status != model.StatusOpen {
		t.Errorf("D-2 status = %s, want open", d.Status)
	}
}

func TestRequestNoPrecedentAcrossRepos(t *testing.T) {
	f := newFixture(t)
	f.decided(otherInRepoA, webhookQ)
	f.caller.Repo = "/r/b"
	m := f.mustCall("request_decision", ticket(webhookQ))
	if ids := precedentIDs(t, m); len(ids) != 0 {
		t.Errorf("precedent across repos = %v, want none", ids)
	}
}

func TestRequestGlobalPrecedentAcrossRepos(t *testing.T) {
	f := newFixture(t)
	f.decided(otherInRepoA, webhookQ, set("scope", "global"))
	f.caller.Repo = "/r/b"
	m := f.mustCall("request_decision", ticket(webhookQ))
	if ids := precedentIDs(t, m); !slices.Equal(ids, []string{"D-1"}) {
		t.Errorf("global precedent = %v, want [D-1]", ids)
	}
}

func TestRequestWeakMatchNotReturned(t *testing.T) {
	f := newFixture(t)
	f.decided(otherInRepoA, webhookQ)
	m := f.mustCall("request_decision", ticket("Should the cache live in Postgres or Redis?"))
	if ids := precedentIDs(t, m); len(ids) != 0 {
		t.Errorf("weak match = %v, want none", ids)
	}
	// The pool is real: search finds D-1 for the same words.
	s := f.mustCall("search_decisions", map[string]any{"query": "Should the cache live in Postgres or Redis?"})
	if len(typed[[]any](t, s["decisions"])) != 1 {
		t.Errorf("search = %v, want D-1", s)
	}
}

func TestDiffersFromSuppressesThatPrecedent(t *testing.T) {
	f := newFixture(t)
	f.decided(otherInRepoA, webhookQ)
	q := "Use Postgres or Redis to store the webhook events?"
	if ids := precedentIDs(t, f.mustCall("request_decision", ticket(q))); !slices.Equal(ids, []string{"D-1"}) {
		t.Fatalf("first precedent = %v, want [D-1]", ids)
	}
	m := f.mustCall("request_decision", ticket(q, set("differs_from", "D-1"), set("differs_why", "these events are ephemeral")))
	if m["id"] != "D-2" || m["duplicate"] != true || len(precedentIDs(t, m)) != 0 {
		t.Errorf("retry with differs_from = %v, want D-2 duplicate without precedent", m)
	}
	if d := f.get("D-2"); d.DiffersFrom != "D-1" || d.DiffersWhy != "these events are ephemeral" {
		t.Errorf("D-2 differs = %q %q", d.DiffersFrom, d.DiffersWhy)
	}
	// A new ticket naming D-1 up front does not get D-1 listed.
	f.caller.SessionID = "S2"
	m = f.mustCall("request_decision", ticket(q, set("differs_from", "D-1"), set("differs_why", "ephemeral")))
	if m["id"] != "D-3" || len(precedentIDs(t, m)) != 0 || m["instruction"] == "" {
		t.Errorf("new ticket with differs_from = %v", m)
	}
	if strings.Contains(typed[string](t, m["instruction"]), "past decision") {
		t.Errorf("instruction = %q, want the blocking one", m["instruction"])
	}

	terr := f.wantError("request_decision", ticket("Another one entirely?", set("differs_from", "D-99"), set("differs_why", "x")), "bad_precedent")
	if len(terr.Problems) != 1 || terr.Problems[0].Field != "differs_from" {
		t.Errorf("bad_precedent problems = %+v", terr.Problems)
	}
	if ds, _ := f.st.ListDecisions(context.Background(), store.DecisionFilter{}); len(ds) != 3 {
		t.Errorf("rows = %d, want 3 (no row for the refused ticket)", len(ds))
	}
}

func TestTicketWithoutSessionIsAccepted(t *testing.T) {
	f := newFixture(t)
	f.caller = Caller{}
	m := f.mustCall("request_decision", ticket(webhookQ))
	if m["id"] != "D-1" || m["status"] != "open" {
		t.Fatalf("result = %v", m)
	}
	if d := f.get("D-1"); d.SessionID != "" || d.Repo != "" {
		t.Errorf("row session %q repo %q, want empty", d.SessionID, d.Repo)
	}
}

func TestBlockingNotifiesOnce(t *testing.T) {
	f := newFixture(t)
	f.mustCall("request_decision", ticket(webhookQ))
	if len(f.notified) != 1 || f.notified[0].d.ID != "D-1" || f.notified[0].d.Mode != model.ModeBlocking ||
		f.notified[0].d.Question != webhookQ || f.notified[0].pane != "%3" {
		t.Fatalf("notified = %+v, want D-1 in %%3", f.notified)
	}
	f.mustCall("request_decision", ticket(webhookQ))                                      // duplicate
	f.mustCall("request_decision", ticket("Which queue for retries?", assume))            // assume
	f.wantError("request_decision", ticket(webhookQ, set("mode", "x")), "invalid_ticket") // refused
	if len(f.notified) != 1 {
		t.Errorf("notified %d times, want once", len(f.notified))
	}
}

func TestNilNotifyIsAllowed(t *testing.T) {
	f := newFixture(t)
	tools := Tools(Deps{
		Store: f.st, Cfg: config.Default(), Now: func() time.Time { return toolsT0 },
		Caller: func(context.Context) Caller { return Caller{} },
	})
	if _, terr := tools[1].Call(context.Background(), mustJSON(t, ticket(webhookQ))); terr != nil {
		t.Fatalf("request with nil Notify and nil Log: %+v", terr)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCheckReturnsAnswerAndClaims(t *testing.T) {
	f := newFixture(t)
	f.mustCall("request_decision", ticket(webhookQ))
	f.answer("D-1", "B", "we already run Redis")
	if d := f.get("D-1"); d.Delivery != model.DeliveryQueued {
		t.Fatalf("delivery before check = %s, want queued", d.Delivery)
	}
	m := f.mustCall("check_decision", map[string]any{"id": "D-1"})
	want := map[string]any{
		"id": "D-1", "status": "answered", "mode": "blocking", "answer_key": "B", "answer": "B: Redis",
		"reasoning": "we already run Redis", "withdraw_reason": "", "instruction": "Decided. Follow the answer.",
	}
	if !reflect.DeepEqual(m, want) {
		t.Errorf("check = %v, want %v", m, want)
	}
	if d := f.get("D-1"); d.Delivery != model.DeliveryDelivered || d.DeliveredVia != model.ViaCheck {
		t.Errorf("after check delivery = %s via %s, want delivered via check", d.Delivery, d.DeliveredVia)
	}
	// Asking again still returns the answer already delivered.
	if m := f.mustCall("check_decision", map[string]any{"id": "D-1"}); m["answer_key"] != "B" {
		t.Errorf("second check = %v", m)
	}
}

func TestCheckByOtherSessionDoesNotClaim(t *testing.T) {
	f := newFixture(t)
	f.mustCall("request_decision", ticket(webhookQ))
	f.answer("D-1", "B", "")
	f.caller = Caller{SessionID: "S2", Repo: "/r/a"}
	m := f.mustCall("check_decision", map[string]any{"id": "D-1"})
	if m["status"] != "answered" {
		t.Errorf("check by S2 = %v", m)
	}
	if d := f.get("D-1"); d.Delivery != model.DeliveryQueued {
		t.Errorf("delivery after another session's check = %s, want queued", d.Delivery)
	}
	f.caller = Caller{}
	f.mustCall("check_decision", map[string]any{"id": "D-1"})
	if d := f.get("D-1"); d.Delivery != model.DeliveryQueued {
		t.Errorf("delivery after an unknown caller's check = %s, want queued", d.Delivery)
	}
}

func TestCheckInstructionPerStatus(t *testing.T) {
	f := newFixture(t)
	f.mustCall("request_decision", ticket(webhookQ))
	if m := f.mustCall("check_decision", map[string]any{"id": "D-1"}); m["instruction"] != "Still open." || m["status"] != "open" {
		t.Errorf("open check = %v", m)
	}
	f.mustCall("withdraw_decision", map[string]any{"id": "D-1", "reason": "not needed"})
	m := f.mustCall("check_decision", map[string]any{"id": "D-1"})
	if m["instruction"] != "Closed without an answer." || m["withdraw_reason"] != "not needed" {
		t.Errorf("withdrawn check = %v", m)
	}
	f.mustCall("request_decision", ticket("Which queue for retries?", assume))
	f.answer("D-2", "B", "the queue is gone")
	m = f.mustCall("check_decision", map[string]any{"id": "D-2"})
	if m["status"] != "overridden" || m["instruction"] != "Overridden. Revert work based on your assumption, then follow the answer." {
		t.Errorf("overridden check = %v", m)
	}
	f.mustCall("request_decision", ticket("Which port?", assume))
	f.answer("D-3", "A", "")
	if m := f.mustCall("check_decision", map[string]any{"id": "D-3"}); m["status"] != "confirmed" || m["instruction"] != "Decided. Follow the answer." {
		t.Errorf("confirmed check = %v", m)
	}
}

func TestCheckUnknownAndMalformedID(t *testing.T) {
	f := newFixture(t)
	f.wantError("check_decision", map[string]any{"id": "D-99"}, "not_found")
	for _, args := range []string{`{"id":"117"}`, `{"id":"D-1; x"}`, `{"id":"D-0"}`, `{"id":7}`, `{"id":null}`, `{}`, `{"id":["D-1"]}`} {
		terr := f.wantError("check_decision", args, "invalid_arguments")
		if len(terr.Problems) != 1 || terr.Problems[0].Field != "id" {
			t.Errorf("%s problems = %+v, want one on id", args, terr.Problems)
		}
		if strings.Contains(terr.Message, "D-1; x") {
			t.Errorf("error echoes the argument: %q", terr.Message)
		}
	}
}

func TestWithdrawByOwnerCitingPrecedent(t *testing.T) {
	f := newFixture(t)
	f.decided(otherInRepoA, webhookQ)
	f.mustCall("request_decision", ticket("Use Postgres or Redis to store the webhook events?"))
	m := f.mustCall("withdraw_decision", map[string]any{"id": "D-2", "reason": " settled by\x1b[2J D-1 ", "precedent_id": "D-1"})
	if want := map[string]any{"id": "D-2", "status": "withdrawn"}; !reflect.DeepEqual(m, want) {
		t.Errorf("withdraw = %v, want %v", m, want)
	}
	d := f.get("D-2")
	if d.Status != model.StatusWithdrawn || d.PrecedentOf != "D-1" || d.WithdrawReason != "settled by D-1" {
		t.Errorf("row status %s precedent_of %q reason %q", d.Status, d.PrecedentOf, d.WithdrawReason)
	}
}

func TestWithdrawErrors(t *testing.T) {
	f := newFixture(t)
	f.mustCall("request_decision", ticket(webhookQ))
	f.as(Caller{SessionID: "S2"}, func() {
		f.wantError("withdraw_decision", map[string]any{"id": "D-1", "reason": "mine now"}, "not_yours")
	})
	f.as(Caller{}, func() {
		f.wantError("withdraw_decision", map[string]any{"id": "D-1", "reason": "mine now"}, "not_yours")
	})
	f.wantError("withdraw_decision", map[string]any{"id": "D-1", "reason": "x", "precedent_id": "D-99"}, "bad_precedent")
	f.wantError("withdraw_decision", map[string]any{"id": "D-1", "reason": "x", "precedent_id": "D-1"}, "bad_precedent")
	f.wantError("withdraw_decision", map[string]any{"id": "D-99", "reason": "x"}, "not_found")
	for _, args := range []string{
		`{"id":"D-1","reason":""}`, `{"id":"D-1","reason":"  \n "}`, `{"id":"D-1"}`,
		`{"id":"D-1","reason":"` + strings.Repeat("r", 301) + `"}`, `{"id":"D-1","reason":5}`,
		`{"id":"1","reason":"x"}`, `{"id":"D-1","reason":"x","precedent_id":"D-1 or so"}`, `{"id":"D-1","reason":"x","precedent_id":3}`,
	} {
		f.wantError("withdraw_decision", args, "invalid_arguments")
	}
	if d := f.get("D-1"); d.Status != model.StatusOpen {
		t.Fatalf("D-1 status = %s after refused withdrawals, want open", d.Status)
	}
	f.answer("D-1", "A", "")
	f.wantError("withdraw_decision", map[string]any{"id": "D-1", "reason": "too late"}, "not_open")
}

func TestSearchUsesCallerRepoByDefault(t *testing.T) {
	f := newFixture(t)
	f.decided(otherInRepoA, webhookQ, set("tags", []string{"DB", "events"}))
	f.decided(Caller{SessionID: "S9", Repo: "/r/b"}, "Store webhook events in SQLite or Redis?")
	m := f.mustCall("search_decisions", map[string]any{"query": "webhook events storage"})
	ds := typed[[]any](t, m["decisions"])
	if len(ds) != 1 {
		t.Fatalf("search = %v, want only D-1", m)
	}
	p := typed[map[string]any](t, ds[0])
	if p["id"] != "D-1" || p["repo"] != "a" || !reflect.DeepEqual(p["tags"], []any{"db", "events"}) || p["scope"] != "repo" {
		t.Errorf("P = %v", p)
	}
	if _, err := time.Parse(time.RFC3339, typed[string](t, p["answered_at"])); err != nil || !strings.HasSuffix(typed[string](t, p["answered_at"]), "Z") {
		t.Errorf("answered_at = %v, want RFC 3339 UTC", p["answered_at"])
	}
	m = f.mustCall("search_decisions", map[string]any{"query": "webhook events storage", "repo": "/r/b/"})
	if ds := typed[[]any](t, m["decisions"]); len(ds) != 1 || typed[map[string]any](t, ds[0])["id"] != "D-2" {
		t.Errorf("search with repo /r/b = %v, want only D-2", m)
	}
	m = f.mustCall("search_decisions", map[string]any{"query": "webhook events", "tags": []string{"nope"}})
	if ds := typed[[]any](t, m["decisions"]); len(ds) != 0 {
		t.Errorf("search with a tag nobody has = %v", m)
	}
	m = f.mustCall("search_decisions", map[string]any{"query": "webhook events", "tags": []string{" DB "}})
	if ds := typed[[]any](t, m["decisions"]); len(ds) != 1 {
		t.Errorf("search with tag DB = %v, want D-1", m)
	}
	m = f.mustCall("search_decisions", map[string]any{"query": "the of what"})
	if ds, ok := m["decisions"].([]any); !ok || len(ds) != 0 {
		t.Errorf("stopword-only search = %v, want an empty array", m)
	}
}

func TestSearchTaskScopeIsTheCallersSession(t *testing.T) {
	f := newFixture(t)
	f.decided(f.caller, webhookQ, set("scope", "task"))
	if ds := typed[[]any](t, f.mustCall("search_decisions", map[string]any{"query": webhookQ})["decisions"]); len(ds) != 1 {
		t.Errorf("own session search = %v, want D-1", ds)
	}
	f.caller.SessionID = "S2"
	if ds := typed[[]any](t, f.mustCall("search_decisions", map[string]any{"query": webhookQ})["decisions"]); len(ds) != 0 {
		t.Errorf("other session search = %v, want none", ds)
	}
}

func TestSearchReturnsAtMostFive(t *testing.T) {
	f := newFixture(t)
	for i := range 7 {
		f.decided(Caller{SessionID: "S" + string(rune('a'+i)), Repo: "/r/a"}, webhookQ)
	}
	if ds := typed[[]any](t, f.mustCall("search_decisions", map[string]any{"query": webhookQ})["decisions"]); len(ds) != 5 {
		t.Errorf("search returned %d, want 5", len(ds))
	}
}

func TestSearchArgumentErrors(t *testing.T) {
	f := newFixture(t)
	for _, args := range []string{
		`{}`, `{"query":""}`, `{"query":"  \n"}`, `{"query":7}`, `{"query":"` + strings.Repeat("w", 501) + `"}`,
		`{"query":"x","tags":["a","b","c","d","e","f","g","h","i"]}`, `{"query":"x","tags":"db"}`, `{"query":"x","repo":1}`,
	} {
		f.wantError("search_decisions", args, "invalid_arguments")
	}
	f.mustCall("search_decisions", `{"query":"`+strings.Repeat("w", 500)+`"}`)
}

func TestToolsSweepExpiry(t *testing.T) {
	calls := map[string]string{
		"search_decisions":  `{"query":"anything"}`,
		"request_decision":  `{"question":"x"}`,
		"check_decision":    `{"id":"D-99"}`,
		"withdraw_decision": `{"id":"D-99","reason":"x"}`,
	}
	for name, args := range calls {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.mustCall("request_decision", ticket("Which queue for retries?", assume))
			f.now = f.now.Add(25 * time.Hour)
			_, _ = f.call(name, args)
			if d := f.get("D-1"); d.Status != model.StatusExpired {
				t.Errorf("after %s, D-1 status = %s, want expired", name, d.Status)
			}
		})
	}
}

func TestStoreErrorIsInternal(t *testing.T) {
	calls := map[string]string{
		"search_decisions":  `{"query":"webhook events"}`,
		"request_decision":  string(mustJSON(t, ticket(webhookQ))),
		"check_decision":    `{"id":"D-1"}`,
		"withdraw_decision": `{"id":"D-1","reason":"x"}`,
	}
	for name, args := range calls {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			if err := f.st.Close(); err != nil {
				t.Fatal(err)
			}
			terr := f.wantError(name, args, "internal")
			if terr.Message != "curia could not reach its store" || len(terr.Problems) != 0 {
				t.Errorf("internal error = %+v", terr)
			}
			if !strings.Contains(f.log.String(), "database is closed") {
				t.Errorf("log = %q, want the cause", f.log.String())
			}
		})
	}
}
