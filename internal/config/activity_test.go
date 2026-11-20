package config_test

import (
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/config"
)

func TestLoadActivity(t *testing.T) {
	d, err := config.Load(writeConfig(t, ""))
	if err != nil || d.Activity.IdleCap != 30*time.Minute || len(d.Activity.TaskTypes) != 7 {
		t.Fatalf("defaults = %+v, %v", d.Activity, err)
	}
	c, err := config.Load(writeConfig(t, "[activity]\nidle_cap = \"10m\"\ntask_types = [\"feat\"]\n[[activity.rules]]\nclass = \"testing\"\nmatch = \"^./check.sh\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := config.ActivityRule{Class: "testing", Match: "^./check.sh"}
	if c.Activity.IdleCap != 10*time.Minute || len(c.Activity.TaskTypes) != 1 || len(c.Activity.Rules) != 1 || c.Activity.Rules[0] != want {
		t.Errorf("activity = %+v", c.Activity)
	}
}
