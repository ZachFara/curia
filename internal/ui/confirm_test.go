package ui

import (
	"testing"
	"time"
)

// D-2: a y within burstGap of the key before it is refused even after the
// dwell; nothing else protected this half of soon.
func TestConfirmGateRefusesYSoonAfterAnotherKey(t *testing.T) {
	t0 := time.Unix(1000, 0)
	var g confirmGate
	g.open(t0)
	g.keyed(t0.Add(time.Second))
	g.keyed(t0.Add(time.Second + 10*time.Millisecond))
	if !g.soon(t0.Add(time.Second + 10*time.Millisecond)) {
		t.Error("a y 10 ms after another key (dwell passed) was not refused")
	}
	g.keyed(t0.Add(2 * time.Second))
	if g.soon(t0.Add(2 * time.Second)) {
		t.Error("a y long after the key before it was refused")
	}
}

// D-3: x and y in one read never kill; a y inside the dwell, or a y that
// another key follows within burstGap, never kills; a lone y after the dwell does.
func TestKillIsBehindTheConfirmGate(t *testing.T) {
	m, fake, _ := newTest(t, ModePopup, threeSessions()...)
	m, _ = m.Update(key("jjk"))
	m, _ = m.Update(key("xy"))
	if got := calls(fake, "KillPane"); len(got) != 0 {
		t.Fatalf("x and y in one read killed: %q", got)
	}
	m, _ = m.Update(key("esc"))
	m, _ = press(m, "x") // fresh prompt
	dashClock.advance(100 * time.Millisecond)
	m, _ = m.Update(key("y")) // inside the dwell
	if got := calls(fake, "KillPane"); len(got) != 0 {
		t.Fatalf("y inside the dwell killed: %q", got)
	}
	dashClock.advance(quietPeriod + time.Millisecond)
	p := &dashPump{t: t, m: m}
	wait := p.step(key("y"))
	if wait == nil {
		t.Fatal("a lone y after the dwell was not taken")
	}
	dashClock.advance(10 * time.Millisecond) // a key follows within burstGap of the y
	p.m, _ = p.m.Update(key("j"))
	p.m, _ = p.m.Update(wait())
	if got := calls(fake, "KillPane"); len(got) != 0 {
		t.Fatalf("a y that another key followed killed: %q", got)
	}
	dashClock.advance(quietPeriod + time.Millisecond) // the refused y restarted the wait
	_ = confirmY(t, p.m)
	if got := calls(fake, "KillPane"); len(got) != 1 {
		t.Errorf("a lone y after the dwell: KillPane calls %q, want one", got)
	}
}
