package main

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

// Tests of the web page's state-changing routes (spec F26 part 2, owner
// decision D-9): three task moves and a message, behind serve.web_write.

var servewebActionRoutes = []string{"/task/move", "/say/send", "/say/confirm"}

var servewebCSRFField = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// servewebWFix is a fixture with serve.web_write on.
func servewebWFix(t *testing.T) *servewebFix {
	t.Helper()
	return servewebNewFixW(t, true, "http", true)
}

// csrf reads the session's CSRF token off its /say page.
func (f *servewebFix) csrf(cookie string) string {
	f.t.Helper()
	r := f.do(http.MethodGet, "/say", cookie, nil)
	m := servewebCSRFField.FindStringSubmatch(r.body)
	if r.StatusCode != http.StatusOK || m == nil {
		f.t.Fatalf("GET /say = %d with csrf %v, want 200 and a hidden csrf field", r.StatusCode, m)
	}
	return m[1]
}

// post is a POST with the right Origin and the session's CSRF token added.
func (f *servewebFix) post(path, cookie, csrf string, form url.Values) *servewebResp {
	f.t.Helper()
	form.Set("csrf", csrf)
	return f.do(http.MethodPost, path, cookie, form, func(r *http.Request) { r.Header.Set("Origin", f.origin()) })
}

func (f *servewebFix) task(id string) model.Task {
	f.t.Helper()
	tk, ok, err := f.st.GetTask(context.Background(), id)
	if err != nil || !ok {
		f.t.Fatalf("GetTask %s = %v, %v", id, ok, err)
	}
	return tk
}

func (f *servewebFix) addTask(status model.TaskStatus, title string) model.Task {
	f.t.Helper()
	tk, err := f.st.CreateTask(context.Background(), model.Task{Title: title, Body: "the brief", Repo: "/r/api", Status: status, CreatedAt: serveT0.Add(-time.Hour)})
	if err != nil {
		f.t.Fatal(err)
	}
	return tk
}

func (f *servewebFix) messages() []model.Message {
	f.t.Helper()
	ms, err := f.st.ListMessages(context.Background(), store.MessageFilter{})
	if err != nil {
		f.t.Fatal(err)
	}
	return ms
}

// session seeds a live idle agent in a pane the fake tmux knows.
func (f *servewebFix) session(taskID string) {
	const pane = "%1"
	f.t.Helper()
	s := readSession("s1", model.StateIdle, pane)
	s.TaskID, s.UpdatedAt, s.StartedAt = taskID, f.clock(), f.clock().Add(-30*time.Minute)
	if err := f.st.UpsertSession(context.Background(), s); err != nil {
		f.t.Fatal(err)
	}
	f.tm.Panes = append(f.tm.Panes, tmux.Pane{ID: pane, Session: "work", WindowID: "@" + pane[1:], ServerStart: f.clock().Add(-time.Hour)})
	f.tm.Screen = decideIdleScreen
}

func TestServewebWriteOffHasNoRoutesAndNoForms(t *testing.T) {
	f := servewebNewFix(t, true, "http")
	f.addTask(model.TaskBacklog, "a task")
	cookie := f.login(f.device("phone"))
	for _, p := range append([]string{"/say"}, servewebActionRoutes...) {
		for _, m := range []string{http.MethodGet, http.MethodPost} {
			if r := f.do(m, p, cookie, url.Values{"x": {"y"}}, func(r *http.Request) { r.Header.Set("Origin", f.origin()) }); r.StatusCode != http.StatusNotFound {
				t.Errorf("%s %s with serve.web_write off = %d, want 404", m, p, r.StatusCode)
			}
		}
	}
	for _, p := range servewebPages[:5] {
		if b := f.do(http.MethodGet, p, cookie, nil).body; strings.Contains(b, "<form") || strings.Contains(b, "csrf") || strings.Contains(b, `href="/say"`) {
			t.Errorf("%s holds a form, a token or a link to /say with writes off", p)
		}
	}
}

func TestServewebWriteRefusals(t *testing.T) {
	f := servewebWFix(t)
	tk := f.addTask(model.TaskBacklog, "a task")
	cookie := f.login(f.device("phone"))
	good := f.csrf(cookie)
	form := func() url.Values { return url.Values{"id": {tk.ID}, "to": {"ready"}, "csrf": {good}} }
	origin := func(v string) func(*http.Request) { return func(r *http.Request) { r.Header.Set("Origin", v) } }
	for _, p := range servewebActionRoutes {
		for name, c := range map[string]struct {
			cookie string
			edit   func(*http.Request)
			want   int
		}{
			"no cookie":       {"", origin(f.origin()), http.StatusUnauthorized},
			"made-up cookie":  {"curia_session=nonsense", origin(f.origin()), http.StatusUnauthorized},
			"missing Origin":  {cookie, func(*http.Request) {}, http.StatusForbidden},
			"wrong Origin":    {cookie, origin("http://evil.example"), http.StatusForbidden},
			"null Origin":     {cookie, origin("null"), http.StatusForbidden},
			"wrong Host":      {cookie, func(r *http.Request) { r.Header.Set("Origin", f.origin()); r.Host = "evil.example" }, http.StatusForbidden},
			"GET not allowed": {cookie, nil, http.StatusMethodNotAllowed},
		} {
			method, body := http.MethodPost, form()
			if c.edit == nil {
				method, body, c.edit = http.MethodGet, nil, func(*http.Request) {}
			}
			r := f.do(method, p, c.cookie, body, c.edit)
			if r.StatusCode != c.want {
				t.Errorf("%s with %s = %d, want %d", p, name, r.StatusCode, c.want)
			}
			if c.want == http.StatusMethodNotAllowed && r.Header.Get("Allow") != "POST" {
				t.Errorf("GET %s Allow = %q, want POST", p, r.Header.Get("Allow"))
			}
		}
	}
	if r := f.do(http.MethodPost, "/say", cookie, form(), origin(f.origin())); r.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /say = %d, want 405: it is only a page", r.StatusCode)
	}
	if got := f.task(tk.ID); got.Status != model.TaskBacklog || len(f.messages()) != 0 {
		t.Errorf("a refused request changed something: task %s, %d messages", got.Status, len(f.messages()))
	}
}

func TestServewebCSRFTokenIsBoundToItsSession(t *testing.T) {
	f := servewebWFix(t)
	tk := f.addTask(model.TaskBacklog, "a task")
	token := f.device("phone")
	a, b := f.login(token), f.login(token)
	ca, cb := f.csrf(a), f.csrf(b)
	if ca == cb || len(ca) < 43 {
		t.Fatalf("CSRF tokens %q and %q: want two different 256-bit values (rotated on login)", ca, cb)
	}
	for name, tok := range map[string]string{"missing": "", "wrong": strings.Repeat("A", 43), "another session's": cb, "truncated": ca[:20]} {
		form := url.Values{"id": {tk.ID}, "to": {"ready"}}
		if tok != "" {
			form.Set("csrf", tok)
		}
		r := f.do(http.MethodPost, "/task/move", a, form, func(r *http.Request) { r.Header.Set("Origin", f.origin()) })
		if r.StatusCode != http.StatusForbidden {
			t.Errorf("%s token = %d, want 403", name, r.StatusCode)
		}
	}
	if f.task(tk.ID).Status != model.TaskBacklog {
		t.Fatal("a request with a bad token moved the task")
	}
	if r := f.post("/task/move", a, ca, url.Values{"id": {tk.ID}, "to": {"ready"}}); r.StatusCode != http.StatusSeeOther {
		t.Errorf("the right token = %d, want 303", r.StatusCode)
	}
}

func TestServewebReplayAfterRevokeIsRefused(t *testing.T) {
	f := servewebWFix(t)
	tk := f.addTask(model.TaskBacklog, "a task")
	cookie := f.login(f.device("phone"))
	tok := f.csrf(cookie)
	if ok, err := f.st.DeleteDevice(context.Background(), "phone"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if r := f.post("/task/move", cookie, tok, url.Values{"id": {tk.ID}, "to": {"ready"}}); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("replayed form after revoke = %d, want 401", r.StatusCode)
	}
	if f.task(tk.ID).Status != model.TaskBacklog {
		t.Error("a replayed form moved the task")
	}
}

func TestServewebMovesAreExactlyTheTransition(t *testing.T) {
	f := servewebWFix(t)
	tk := f.addTask(model.TaskBacklog, "move me")
	other := f.addTask(model.TaskBacklog, "leave me")
	cookie := f.login(f.device("phone"))
	tok := f.csrf(cookie)
	for _, step := range []struct{ to, from model.TaskStatus }{
		{model.TaskReady, model.TaskBacklog}, {model.TaskBacklog, model.TaskReady}, {model.TaskReady, model.TaskBacklog}, {model.TaskDropped, model.TaskReady},
	} {
		before := f.task(tk.ID)
		if before.Status != step.from {
			t.Fatalf("setup: %s is %s, want %s", tk.ID, before.Status, step.from)
		}
		f.now.Add(int64(time.Second))
		r := f.post("/task/move", cookie, tok, url.Values{"id": {tk.ID}, "to": {string(step.to)}})
		if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/" {
			t.Fatalf("move to %s = %d to %q, want 303 to /", step.to, r.StatusCode, r.Header.Get("Location"))
		}
		after := f.task(tk.ID)
		if after.Status != step.to || !after.UpdatedAt.After(before.UpdatedAt) || after.Title != before.Title || after.Body != before.Body || after.Repo != before.Repo || after.Priority != before.Priority || after.Branch != "" || !after.LaunchedAt.IsZero() {
			t.Errorf("move to %s: %+v -> %+v", step.to, before, after)
		}
	}
	f.untouched(other)
}

// untouched checks that task other is as it was and no message exists.
func (f *servewebFix) untouched(other model.Task) {
	f.t.Helper()
	if got := f.task(other.ID); got.Status != model.TaskBacklog || !got.UpdatedAt.Equal(other.UpdatedAt) {
		f.t.Errorf("another task changed: %+v", got)
	}
	if len(f.messages()) != 0 {
		f.t.Error("a move created a message")
	}
}

func TestServewebIllegalMovesAreRefusedWithTheirReason(t *testing.T) {
	f := servewebWFix(t)
	dropped := f.addTask(model.TaskDropped, "gone\x1b[31m")
	running := f.addTask(model.TaskReady, "busy")
	f.session(running.ID)
	plain := f.addTask(model.TaskBacklog, "plain")
	cookie := f.login(f.device("phone"))
	tok := f.csrf(cookie)
	for _, c := range []struct{ id, to string }{{dropped.ID, "ready"}, {dropped.ID, "backlog"}, {running.ID, "dropped"}} {
		r := f.post("/task/move", cookie, tok, url.Values{"id": {c.id}, "to": {c.to}})
		if r.StatusCode != http.StatusSeeOther {
			t.Errorf("illegal move %s to %s = %d, want 303 with the reason on the page", c.id, c.to, r.StatusCode)
		}
		page := f.do(http.MethodGet, "/", cookie, nil).body
		if !strings.Contains(page, c.id+" is ") || strings.Contains(page, "\x1b") {
			t.Errorf("after %s to %s the page lacks a cleaned reason: %q", c.id, c.to, page)
		}
	}
	if f.task(dropped.ID).Status != model.TaskDropped || f.task(running.ID).Status != model.TaskReady {
		t.Error("an illegal move changed a task")
	}
	// targets that are no drop target, ids that are no task id, and a missing task
	for _, c := range []struct{ id, to string }{{plain.ID, "running"}, {plain.ID, "done"}, {plain.ID, "review"}, {plain.ID, ""}, {"../x", "ready"}, {"T-999", "ready"}, {"", "ready"}} {
		if r := f.post("/task/move", cookie, tok, url.Values{"id": {c.id}, "to": {c.to}}); r.StatusCode == http.StatusOK {
			t.Errorf("move %q to %q = 200", c.id, c.to)
		}
	}
	if f.task(plain.ID).Status != model.TaskBacklog {
		t.Error("a bad target moved the task")
	}
}

func TestServewebForgedFieldsDoNothingElse(t *testing.T) {
	f := servewebWFix(t)
	tk := f.addTask(model.TaskBacklog, "a task")
	d := pushTicket(t, f.st, "a question", model.ReversCheap, 2)
	cookie := f.login(f.device("phone"))
	tok := f.csrf(cookie)
	r := f.post("/task/move", cookie, tok, url.Values{
		"id": {tk.ID}, "to": {"ready"}, "action": {"approve"}, "decide": {d.ID}, "decision": {d.ID}, "key": {"A"}, "review": {"R-1"},
		"approve": {"1"}, "merge": {"1"}, "setting": {"auto_send=true"}, "spawn": {tk.ID},
	})
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("move = %d", r.StatusCode)
	}
	got, _, err := f.st.GetDecision(context.Background(), d.ID)
	if err != nil || got.Status != model.StatusOpen {
		t.Errorf("a forged field touched the ticket: %+v, %v", got, err)
	}
	if g := f.task(tk.ID); g.Status != model.TaskReady || g.Branch != "" || !g.LaunchedAt.IsZero() {
		t.Errorf("the move did more than set the status: %+v", g)
	}
	for _, p := range []string{"/decide", "/review/approve", "/v1/answer/web", "/spawn", "/config", "/schedule", "/mode", "/say/broadcast"} {
		if r := f.post(p, cookie, tok, url.Values{"id": {d.ID}}); r.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404", p, r.StatusCode)
		}
	}
}

// servewebCheckHeld checks ms is one held, cleaned instruction from the user to s1.
func servewebCheckHeld(t *testing.T, ms []model.Message) {
	t.Helper()
	if len(ms) != 1 || ms[0].Status != model.MsgHeld || ms[0].Sender != model.SenderUser || ms[0].TargetSession != "s1" || ms[0].Kind != model.KindInstruction {
		t.Fatalf("messages = %+v, want one held instruction from the user to s1", ms)
	}
	if strings.ContainsAny(ms[0].Text, "\x1b\x07\u202e\u202c") || !strings.Contains(ms[0].Text, "use pnpm") {
		t.Errorf("stored text %q is not cleaned", ms[0].Text)
	}
}

func TestServewebMessageIsHeldUntilConfirmed(t *testing.T) {
	f := servewebWFix(t)
	f.session("")
	cookie := f.login(f.device("phone"))
	tok := f.csrf(cookie)
	dirty := "use pnpm\x1b[31m not npm\x1b]0;x\x07 \u202eevil\u202c"
	r := f.post("/say/send", cookie, tok, url.Values{"target": {"session:s1"}, "text": {dirty}})
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/say" {
		t.Fatalf("send = %d to %q, want 303 to /say", r.StatusCode, r.Header.Get("Location"))
	}
	ms := f.messages()
	servewebCheckHeld(t, ms)
	page := f.do(http.MethodGet, "/say", cookie, nil).body
	servewebCheckClean(t, "/say", page)
	if !strings.Contains(page, "use pnpm") || !strings.Contains(page, ms[0].ID) {
		t.Errorf("the preview page does not show the queued text and its id: %q", page)
	}
	if len(f.tm.Calls) > 0 && strings.Contains(strings.Join(f.tm.Calls, "\n"), "SendKeys") {
		t.Fatal("typed into a pane before the confirmation")
	}
	r = f.post("/say/confirm", cookie, tok, url.Values{"id": {ms[0].ID}})
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("confirm = %d", r.StatusCode)
	}
	if got := f.messages()[0]; got.Status != model.MsgDelivered {
		t.Errorf("after the confirmation the message is %s, want delivered through the guarded path", got.Status)
	}
	if !strings.Contains(strings.Join(f.tm.Calls, "\n"), "SendKeys %1") {
		t.Errorf("no keys were typed: %v", f.tm.Calls)
	}
	f.noRetype(cookie, tok, ms[0].ID)
}

// noRetype confirms id a second time and checks nothing more was typed.
func (f *servewebFix) noRetype(cookie, tok, id string) {
	f.t.Helper()
	n := len(f.tm.Calls)
	f.post("/say/confirm", cookie, tok, url.Values{"id": {id}})
	if len(f.tm.Calls) != n {
		f.t.Error("a replayed confirmation typed again")
	}
}

func TestServewebMessageToATaskAndRefusals(t *testing.T) {
	f := servewebWFix(t)
	tk := f.addTask(model.TaskReady, "busy")
	idle := f.addTask(model.TaskReady, "no agent")
	f.session(tk.ID)
	cookie := f.login(f.device("phone"))
	tok := f.csrf(cookie)
	f.post("/say/send", cookie, tok, url.Values{"target": {"task:" + tk.ID}, "text": {"hello"}})
	if ms := f.messages(); len(ms) != 1 || ms[0].TargetSession != "s1" || ms[0].TargetTask != tk.ID || ms[0].Status != model.MsgHeld {
		t.Fatalf("a task target should settle on its live session: %+v", ms)
	}
	for name, form := range map[string]url.Values{
		"a task with no agent": {"target": {"task:" + idle.ID}, "text": {"hello"}},
		"a repo":               {"target": {"repo:/r/api"}, "text": {"hello"}},
		"global":               {"target": {"global"}, "text": {"hello"}},
		"a broadcast":          {"target": {"broadcast"}, "text": {"hello"}},
		"an unknown session":   {"target": {"session:nope"}, "text": {"hello"}},
		"empty text":           {"target": {"session:s1"}, "text": {"\x1b[31m \u202e"}},
		"text over the bound":  {"target": {"session:s1"}, "text": {strings.Repeat("x", 801)}},
		"a rule":               {"target": {"session:s1"}, "text": {"rule: always x"}, "kind": {"rule"}},
		"no target":            {"text": {"hello"}},
	} {
		if r := f.post("/say/send", cookie, tok, form); r.StatusCode == http.StatusOK {
			t.Errorf("%s = 200", name)
		}
	}
	// a rule-looking text is still just an instruction: no decision row, no task
	for _, m := range f.messages() {
		if m.Kind != model.KindInstruction {
			t.Errorf("message %+v is not an instruction", m)
		}
	}
	if len(f.messages()) > 2 {
		t.Errorf("refused sends stored messages: %+v", f.messages())
	}
}

func TestServewebConfirmRefusesAnExpiredOrForeignMessage(t *testing.T) {
	f := servewebWFix(t)
	f.session("")
	cookie := f.login(f.device("phone"))
	tok := f.csrf(cookie)
	f.post("/say/send", cookie, tok, url.Values{"target": {"session:s1"}, "text": {"hello"}})
	id := f.messages()[0].ID
	f.now.Add(int64(time.Hour))
	f.post("/say/confirm", cookie, tok, url.Values{"id": {id}})
	if got := f.messages()[0]; got.Status == model.MsgDelivered || got.Status == model.MsgQueued {
		t.Errorf("a confirmation after the hold ended released the message: %s", got.Status)
	}
	// a queued message from the terminal cannot be pushed through the confirm route
	m, err := f.st.CreateMessage(context.Background(), model.Message{Text: "x", Kind: model.KindInstruction, Sender: model.SenderUser, TargetSession: "s1", RoutedBy: model.RoutedManual, Status: model.MsgQueued, CreatedAt: f.clock()})
	if err != nil {
		t.Fatal(err)
	}
	f.post("/say/confirm", cookie, tok, url.Values{"id": {m.ID}, "x": {"y"}})
	if got, _, _ := f.st.GetMessage(context.Background(), m.ID); got.Status != model.MsgQueued {
		t.Errorf("confirm acted on a message that was not held: %s", got.Status)
	}
}

func TestServewebBodyIsBounded(t *testing.T) {
	f := servewebWFix(t)
	f.session("")
	cookie := f.login(f.device("phone"))
	tok := f.csrf(cookie)
	for _, form := range []url.Values{
		{"target": {"session:s1"}, "text": {strings.Repeat("a", 1<<20)}},
		{"target": {"session:s1"}, "text": {"hi"}, "pad": {strings.Repeat("b", 1<<20)}},
	} {
		if r := f.post("/say/send", cookie, tok, form); r.StatusCode != http.StatusRequestEntityTooLarge {
			t.Errorf("a %d byte body = %d, want 413", len(form.Encode()), r.StatusCode)
		}
	}
	if len(f.messages()) != 0 {
		t.Error("an oversized body stored a message")
	}
}

func TestServewebWritesAreRateLimitedPerDevice(t *testing.T) {
	f := servewebWFix(t)
	tk := f.addTask(model.TaskBacklog, "a task")
	ca, cb := f.login(f.device("phone")), f.login(f.device("tablet"))
	ta, tb := f.csrf(ca), f.csrf(cb)
	last := 0
	for i := range servewebMaxWrites + 3 {
		r := f.post("/task/move", ca, ta, url.Values{"id": {tk.ID}, "to": {"ready"}})
		last = r.StatusCode
		if i < servewebMaxWrites && r.StatusCode != http.StatusSeeOther {
			t.Fatalf("request %d = %d, want 303 inside the limit", i, r.StatusCode)
		}
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("past the limit = %d, want 429", last)
	}
	if r := f.post("/task/move", cb, tb, url.Values{"id": {tk.ID}, "to": {"backlog"}}); r.StatusCode != http.StatusSeeOther {
		t.Errorf("another device while the first is limited = %d, want 303", r.StatusCode)
	}
	f.now.Add(int64(2 * time.Minute))
	if r := f.post("/task/move", ca, ta, url.Values{"id": {tk.ID}, "to": {"ready"}}); r.StatusCode != http.StatusSeeOther {
		t.Errorf("after the window = %d, want 303", r.StatusCode)
	}
}

func TestServewebWriteLimiterTableIsBounded(t *testing.T) {
	l := servewebNewLimiter(servewebMaxWrites)
	now := serveT0
	for i := range 1000 {
		l.failed(strings.Repeat("d", 1)+string(rune('a'+i%26))+strings.Repeat("e", i/26), now)
	}
	if l.size() > servewebMaxAddrs {
		t.Errorf("the table holds %d entries, want at most %d", l.size(), servewebMaxAddrs)
	}
}

func TestServewebTimelineRecordsTheDevice(t *testing.T) {
	f := servewebWFix(t)
	tk := f.addTask(model.TaskBacklog, "a task")
	f.session("")
	cookie := f.login(f.device("my-phone"))
	tok := f.csrf(cookie)
	f.now.Add(int64(time.Second))
	f.post("/task/move", cookie, tok, url.Values{"id": {tk.ID}, "to": {"ready"}})
	f.post("/say/send", cookie, tok, url.Values{"target": {"session:s1"}, "text": {"hello"}})
	f.post("/say/confirm", cookie, tok, url.Values{"id": {f.messages()[0].ID}})
	page := f.do(http.MethodGet, "/timeline", cookie, nil).body
	for _, want := range []string{"web_action", "my-phone", tk.ID + " ready", "message " + f.messages()[0].ID} {
		if !strings.Contains(page, want) {
			t.Errorf("the timeline lacks %q: %q", want, page)
		}
	}
}

func TestServewebWriteStartLine(t *testing.T) {
	var on, off strings.Builder
	serveWebStartLine(&on, "127.0.0.1:8787", true)
	serveWebStartLine(&off, "127.0.0.1:8787", false)
	if !strings.Contains(on.String(), "writes are on") || !strings.Contains(off.String(), "writes are off") {
		t.Errorf("start lines %q and %q do not say whether writes are on", on.String(), off.String())
	}
}
