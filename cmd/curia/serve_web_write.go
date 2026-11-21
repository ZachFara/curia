package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ZachFara/curia/internal/deliver"
	"github.com/ZachFara/curia/internal/message"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/route"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/task"
	"github.com/ZachFara/curia/internal/ui"
)

// The web page's state-changing routes (spec F26 part 2, owner decision D-9):
// three task moves (to ready, to backlog, to dropped) and one message to a
// session or a task, held until a second request confirms it. Nothing else
// changes from here: no answer, review, spawn, setting, mode or schedule, and
// no broadcast (see servewebSend).
//
// Why a device token may do this although the commands are user-only: the
// user guard (userOnly) tells the user at a terminal from an agent. Here the
// actor is the user on a device they enrolled with `curia serve token`, shown
// once at that terminal; the cookie, the exact Origin and the per-session CSRF
// token prove that device and that page. An agent cannot get a device token
// (only its hash is stored), so the guard's intent, that an agent never acts as
// the user, holds.

// servewebWriteRoutes are the routes that exist only with serve.web_write; the
// value is whether the route is an action (POST only) rather than a page.
var servewebWriteRoutes = map[string]bool{"/say": false, "/task/move": true, "/say/send": true, "/say/confirm": true}

const servewebHold = 10 * time.Minute // how long a message waits for its confirmation

type servewebTarget struct{ Ref, Label string }

type servewebHeld struct{ ID, Target, Text string }

// servewebMovesFrom are the drop targets a card in column st may go to.
func servewebMovesFrom(st model.TaskStatus) []string {
	switch st {
	case model.TaskBacklog:
		return []string{"ready", "dropped"}
	case model.TaskReady:
		return []string{"backlog", "dropped"}
	}
	return nil
}

// action runs one state-changing request: POST only, a present and exact
// Origin, a live session, the device's rate limit, a bounded body and the
// session's CSRF token, in that order; the answer is always a 303 back to the
// page, with the result kept on the session, never in the URL.
func (w *serveWeb) action(rw http.ResponseWriter, r *http.Request, route string) {
	key, sess, now, ok := w.guard(rw, r, route)
	if !ok {
		return
	}
	var result string
	var bad bool
	switch route {
	case "/task/move":
		result, bad = w.move(r.Context(), sess.device, r.PostForm.Get("id"), r.PostForm.Get("to"), now)
	case "/say/send":
		result = w.send(r.Context(), sess.device, r.PostForm.Get("target"), r.PostForm.Get("text"), now)
	case "/say/confirm":
		result = w.confirm(r.Context(), sess.device, r.PostForm.Get("id"), now)
	}
	if bad {
		w.text(rw, r, route, http.StatusBadRequest, "bad request")
		return
	}
	w.mu.Lock()
	if s, ok := w.sessions[key]; ok {
		s.flash, s.flashUntil = result, now.Add(time.Minute)
		w.sessions[key] = s
	}
	w.mu.Unlock()
	back := "/"
	if route != "/task/move" {
		back = "/say"
	}
	rw.Header().Set("Location", back)
	w.reply(rw, r, route, http.StatusSeeOther, "", nil)
}

// guard is every check before an action: method, Origin, session, rate
// limit, body bound and CSRF token. It answers a refusal itself.
func (w *serveWeb) guard(rw http.ResponseWriter, r *http.Request, route string) (key [32]byte, sess servewebSession, now time.Time, ok bool) {
	if r.Method != http.MethodPost {
		rw.Header().Set("Allow", "POST")
		w.text(rw, r, route, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if len(r.Header.Values("Origin")) != 1 { // originOK has checked its value; a POST must carry one
		w.text(rw, r, route, http.StatusForbidden, "forbidden")
		return
	}
	key, sess, authed := w.authed(r)
	if !authed {
		w.text(rw, r, route, http.StatusUnauthorized, "unauthorized: log in at /login")
		return
	}
	now = w.Now()
	if w.wlim.limited(sess.device, now) {
		w.text(rw, r, route, http.StatusTooManyRequests, "too many requests")
		return
	}
	w.wlim.failed(sess.device, now) // counts every request that got this far
	r.Body = http.MaxBytesReader(rw, r.Body, servewebMaxWriteBody)
	if err := r.ParseForm(); err != nil {
		code := http.StatusBadRequest
		var big *http.MaxBytesError
		if errors.As(err, &big) {
			code = http.StatusRequestEntityTooLarge
		}
		w.text(rw, r, route, code, "bad request")
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.PostForm.Get("csrf")), []byte(sess.csrf)) != 1 {
		w.text(rw, r, route, http.StatusForbidden, "forbidden")
		return
	}
	return key, sess, now, true
}

// record writes what device did into the existing events table as a
// WebAction row (no new column); the timeline reads it as "web_action".
func (w *serveWeb) record(ctx context.Context, device, taskID, action string, now time.Time) {
	b, _ := json.Marshal(map[string]string{"device": device, "task": taskID, "action": action})
	if err := w.Store.AppendEvent(ctx, model.Event{Kind: "WebAction", At: now}, b); err != nil {
		w.Log.Warn("serve: web action not recorded", "err", sanitize.Line(err.Error(), 200))
	}
}

// move sets a task's stored status to ready, backlog or dropped, only from
// backlog or ready and only while no agent works on it, as the board's `r` key
// does. bad is a malformed request (400); a refusal is the result's text.
func (w *serveWeb) move(ctx context.Context, device, id, to string, now time.Time) (result string, bad bool) {
	dest := model.TaskStatus(to)
	if !task.ValidID(id) || (dest != model.TaskReady && dest != model.TaskBacklog && dest != model.TaskDropped) {
		return "", true
	}
	say := func(format string, a ...any) string { return servewebText(fmt.Sprintf(format, a...), 300) }
	cards, err := taskCards(ctx, w.Store, now)
	if err != nil {
		return say("%s not moved: %v", id, err), false
	}
	for _, c := range cards {
		if c.Task.ID == id && c.Status != model.TaskBacklog && c.Status != model.TaskReady {
			return say("%s is %s; the page moves a task only between backlog, ready and dropped, from backlog or ready", id, c.Status), false
		}
	}
	_, err = w.Store.UpdateTask(ctx, id, func(cur model.Task) (model.Task, error) {
		if cur.Status != model.TaskBacklog && cur.Status != model.TaskReady {
			return model.Task{}, fmt.Errorf("%s is %s; the page moves a task only from backlog or ready", cur.ID, cur.Status)
		}
		if cur.Status == dest {
			return model.Task{}, fmt.Errorf("%s is already %s", cur.ID, cur.Status)
		}
		cur.Status, cur.UpdatedAt = dest, now
		return cur, nil
	})
	if errors.Is(err, store.ErrNoTask) {
		return say("%s: no such task", id), false
	}
	if err != nil {
		return say("%v", err), false
	}
	w.record(ctx, device, id, id+" "+to, now)
	return say("%s is now %s", id, to), false
}

// send stores a message held (not sent) for a live session, or for a task's
// live session, with the text cleaned and bounded as `curia say` does. There is
// no broadcast, rule or task kind from the page: the target must be one
// session or one task, and the kind is always an instruction.
func (w *serveWeb) send(ctx context.Context, device, target, text string, now time.Time) string {
	clean, err := message.Normalize(text)
	if err != nil {
		return servewebText(sayTextError().Error(), 200)
	}
	cands, _, err := sayCandidates(ctx, w.Store, now)
	if err != nil {
		return servewebText("not queued: "+err.Error(), 200)
	}
	for _, c := range cands {
		if c.Ref != target || (c.Type != route.TargetSession && c.Type != route.TargetTask) {
			continue
		}
		t, err := route.Settle(model.KindInstruction, c, cands)
		if err != nil {
			return servewebText("not queued: "+saySettleError(err).Error(), 200)
		}
		m := sayPlanRow(sayPlan{}, clean, model.KindInstruction, model.MsgHeld, model.RoutedManual, now)
		m.TargetSession, m.TargetTask, m.TargetRepo, m.HoldUntil = t.Session, t.Task, c.Repo, now.Add(servewebHold)
		if m.TargetTask == "" {
			m.TargetTask = c.Session.TaskID
		}
		stored, err := w.Store.CreateMessage(ctx, m)
		if err != nil {
			return servewebText("not queued: "+err.Error(), 200)
		}
		w.record(ctx, device, stored.TargetTask, "message "+stored.ID+" held", now)
		return servewebText(stored.ID+" is held: confirm it below to send it", 200)
	}
	return "not queued: that target is not a live session or a task with one"
}

// confirm releases one held web message and delivers it through the one
// guarded path (deliver.Message). A message that is not held, or whose hold
// ended, is refused; the store's own sweep may not have run.
func (w *serveWeb) confirm(ctx context.Context, device, id string, now time.Time) string {
	m, found, err := w.Store.GetMessage(ctx, id)
	if err != nil || !found || m.Status != model.MsgHeld || m.Sender != model.SenderUser || m.Kind != model.KindInstruction || !now.Before(m.HoldUntil) {
		return servewebText(id+" is not waiting for a confirmation", 100)
	}
	if _, err = w.Store.ReleaseMessage(ctx, id); err != nil {
		return servewebText(id+" is not waiting for a confirmation", 100)
	}
	w.record(ctx, device, m.TargetTask, "message "+id+" confirmed", now)
	out, err := deliver.Message(ctx, w.Store, w.Tmux, w.Log, id, now)
	if err != nil {
		return servewebText(id+" queued for the agent's next stop; typing failed: "+err.Error(), 300)
	}
	if out.Note != "" {
		return servewebText(id+" "+string(out.Message.Status)+": "+out.Note, 300)
	}
	return servewebText(id+" "+string(out.Message.Status), 100)
}

// sayPage is the compose form and the held messages waiting for a confirmation.
func (w *serveWeb) sayPage(ctx context.Context, now time.Time) (servewebPage, error) {
	p := servewebPage{Kind: "say", Title: "Message"}
	cands, _, err := sayCandidates(ctx, w.Store, now)
	if err != nil {
		return p, err
	}
	labels := map[string]string{}
	for _, c := range cands {
		if c.Type == route.TargetSession || c.Type == route.TargetTask {
			p.Targets = append(p.Targets, servewebTarget{c.Ref, servewebText(c.Label, servewebTextLimit)})
			labels[c.Ref] = servewebText(c.Label, servewebTextLimit)
		}
	}
	held, err := w.Store.ListMessages(ctx, store.MessageFilter{Statuses: []model.MessageStatus{model.MsgHeld}})
	for _, m := range held {
		if m.Sender != model.SenderUser || !now.Before(m.HoldUntil) {
			continue
		}
		to := "session:" + m.TargetSession
		p.Held = append(p.Held, servewebHeld{servewebText(m.ID, 20), labels[to], ui.Cell(sanitize.Line(m.Text, message.MaxText))})
	}
	return p, err
}
