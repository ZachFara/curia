package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

// resourcesWorld has three live sessions: s1 sampled 5 s ago, s2 sampled 10
// minutes ago (the monitor stopped), s3 never; and an ended one with a sample.
func resourcesWorld(t *testing.T) *store.Store {
	t.Helper()
	db, _ := fleetHome(t, nil,
		model.Session{ID: "s1", State: model.StateWorking, UpdatedAt: fleetNow},
		model.Session{ID: "s2", State: model.StateIdle, UpdatedAt: fleetNow},
		model.Session{ID: "s3", State: model.StateIdle, UpdatedAt: fleetNow},
		model.Session{ID: "old", State: model.StateEnded, UpdatedAt: fleetNow})
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(); resourcesJSON = false })
	if err = st.AddResourceSamples(context.Background(), []store.ResourceSample{
		{SessionID: "s1", At: fleetNow.Add(-5 * time.Second), CPU: 12.6, RSSMB: 340},
		{SessionID: "s2", At: fleetNow.Add(-10 * time.Minute), CPU: 99, RSSMB: 9999},
		{SessionID: "old", At: fleetNow.Add(-5 * time.Second), CPU: 50, RSSMB: 5000},
	}); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestDashboardCellsShowOnlyCurrentSamples(t *testing.T) {
	st := resourcesWorld(t)
	b, err := storeSource{st: st, tm: &tmux.Fake{}, cfg: config.Default()}.Budget(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if b.Res == nil || b.Res["s1"] != "13% 340M" || len(b.Res) != 1 {
		t.Errorf("Res = %v, want only s1's current cell (stale s2, unsampled s3 and ended sessions get none)", b.Res)
	}
}

func TestResourcesCommand(t *testing.T) {
	resourcesWorld(t)
	out, err := runRoot(t, "resources")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"s1", "13%", "340M", "total"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"99%", "9999", "old", "5000"} {
		if strings.Contains(out, bad) {
			t.Errorf("output shows %q (a stale or ended session's number):\n%s", bad, out)
		}
	}
	if !strings.Contains(out, "monitor not running") {
		t.Errorf("output does not say the monitor is not running:\n%s", out)
	}
	out, err = runRoot(t, "resources", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Sessions []struct {
			ID    string   `json:"id"`
			Fresh bool     `json:"fresh"`
			CPU   *float64 `json:"cpu_pct"`
			RSSMB *int64   `json:"rss_mb"`
		} `json:"sessions"`
		Total          struct{ CPU float64 }
		MonitorRunning bool `json:"monitor_running"`
	}
	if err = json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(got.Sessions) != 3 || got.MonitorRunning {
		t.Fatalf("json = %+v", got)
	}
	for _, s := range got.Sessions {
		if s.Fresh != (s.ID == "s1") || (s.CPU != nil) != s.Fresh || (s.RSSMB != nil) != s.Fresh {
			t.Errorf("session %+v: numbers must be present exactly when fresh, and only s1 is", s)
		}
	}
}

func TestDetailShowsResourcesOrAQuestionMark(t *testing.T) {
	st := resourcesWorld(t)
	src := detailSource{st: st, cfg: config.Default()}
	for id, want := range map[string]string{"s1": "13% 340M", "s2": "?", "s3": "?"} {
		d, err := src.Detail(context.Background(), id)
		if err != nil || !strings.Contains(d.Resources, want) || (want == "?" && strings.Contains(d.Resources, "9999")) {
			t.Errorf("%s: Resources = %q, %v, want it to hold %q", id, d.Resources, err, want)
		}
	}
}
