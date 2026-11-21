package config

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

// wantPolicy is the decision for every key, made here on purpose: a new key
// is not covered until it is named in this table, and the table must match
// keyPolicy. A repository file may only tighten, never loosen.
var wantPolicy = map[string]Policy{
	"db_path": PolicyNone, "log_path": PolicyNone, "context_max": PolicyNone, "default_context_max": PolicyNone,
	"context_warn_pct": PolicyNone, "context_crit_pct": PolicyNone, "stale_hide_after": PolicyNone, "assume_expiry": PolicyNone,
	"notify_bell": PolicyNone, "notify_macos": PolicyNone,
	"routing.auto_send": PolicyFalse, "routing.auto_threshold": PolicyRaise, "routing.aliases": PolicyNone,
	"key_dashboard": PolicyNone, "key_decisions": PolicyNone, "key_next": PolicyNone, "key_message": PolicyNone,
	"editor": PolicyNone, "review.approve": PolicyNone, "review.remote": PolicyNone, "review.diff_tool": PolicyNone,
	"ntfy.url": PolicyNone, "ntfy.quiet": PolicyNone, "ntfy.devices": PolicyNone,
	"serve.listen": PolicyNone, "serve.public_url": PolicyNone, "serve.web": PolicyNone, "serve.web_write": PolicyNone, "serve.web_session": PolicyNone,
	"restore.max_age": PolicyNone, "usage.cheaper_model": PolicyNone, "obsidian.dir": PolicyNone, "linear.url": PolicyNone, "linear.team": PolicyNone,
	"costlens.analyzer": PolicyNone, "cost.task_budget_default": PolicyLower, "cost.budget_hard_stop": PolicyTrue,
	"activity.idle_cap": PolicyNone, "activity.task_types": PolicyNone, "activity.rules": PolicyRepoRules,
	"resources.enabled": PolicyNone, "resources.interval": PolicyNone, "monitor.autostart": PolicyNone,
	"resources.max_containers_per_session": PolicyLower, "resources.max_containers_total": PolicyLower, "resources.docker_memory_refuse_pct": PolicyLower,
	"resources.container_memory_mb": PolicyLower, "resources.container_cpus": PolicyLower,
	"tasks.agent_filed.level": PolicyLevel, "tasks.agent_filed.max_per_day": PolicyLower,
	"schedule.late_window": PolicyLower, "schedule.reset_grace": PolicyLower, "schedule.limited_pct": PolicyNone,
	"rc.launch": PolicyNone, "mode.default": PolicyNone, "mode.away_allow": PolicyNone, "mode.bypass_for": PolicyNone, "away.prepare_wait": PolicyNone,
	"tasks.auto_spawn.enabled": PolicyFalse, "tasks.auto_spawn.max_agents": PolicyLower, "tasks.auto_spawn.max_per_day": PolicyLower, "tasks.auto_spawn.interval": PolicyRaise,
}

// looseValue is a value for key that the policy says a repository must not set: a change for a
// key it cannot touch, the loosening direction for the others. ok is false for lists and tables.
func looseValue(l leaf, p Policy, def Config) (string, bool) {
	v := reflect.ValueOf(def).FieldByIndex(l.index)
	switch {
	case v.Kind() == reflect.Map || v.Kind() == reflect.Slice:
		return "", false
	case v.Kind() == reflect.Bool:
		return fmt.Sprint(p == PolicyFalse || (p == PolicyNone && !v.Bool())), true // False: true; True: false; None: a change
	case v.Type() == durationType:
		d := time.Duration(v.Int())
		if p == PolicyRaise {
			return (d / 2).String(), true
		}
		return (d*2 + time.Second).String(), true
	case v.CanInt():
		if p == PolicyRaise {
			return fmt.Sprint(v.Int() - 1), true
		}
		return fmt.Sprint(v.Int() + 1), true
	case v.CanFloat():
		if p == PolicyRaise {
			return fmt.Sprint(v.Float() / 2), true
		}
		return fmt.Sprint(v.Float()/2 + 1), true
	case p == PolicyLevel:
		return "ready", true
	}
	if x, ok := stringValues[l.key]; ok {
		return x, true
	}
	return "changed", true
}

// stringValues are valid values, other than the default, for the keys whose text is checked.
var stringValues = map[string]string{
	"costlens.analyzer": "/bin/evil.js", "linear.team": "ABC", "linear.url": "https://evil.example", "mode.default": "edits",
	"ntfy.quiet": "01:00-02:00", "ntfy.url": "https://evil.example", "obsidian.dir": "/tmp/vault", "review.approve": "pr",
	"review.diff_tool": "delta", "serve.listen": "127.0.0.1:1", "serve.public_url": "https://evil.example",
}

// A repository file can never loosen any key: every registered key is in the table above, matches keyPolicy,
// and the loosening value of each is refused when a repository file sets it.
func TestRepoFileCannotLoosenAnyKey(t *testing.T) {
	def := Default()
	tried := 0
	for _, k := range Keys() {
		want, ok := wantPolicy[k]
		if !ok {
			t.Errorf("config key %q is not in wantPolicy: decide whether a repository may tighten it", k)
			continue
		}
		if keyPolicy[k] != want {
			t.Errorf("%s: keyPolicy is %d, the table says %d", k, keyPolicy[k], want)
		}
		l, _ := find(k)
		val, ok := looseValue(l, want, def)
		if !ok || want == PolicyRepoRules || (want == PolicyLevel && reflect.ValueOf(def).FieldByIndex(l.index).String() == "ready") {
			continue
		}
		repo, err := UpdateFile(nil, k, val)
		if err != nil {
			t.Errorf("%s: the test value %q is not valid: %v", k, val, err)
			continue
		}
		c, rep, err := Resolve(layerRepo(t, "", string(repo)))
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := Get(c, k); rep.Sources[k] == SourceRepo || !refused(rep, k) {
			t.Errorf("%s: a repository file set %q and it took effect (now %s, source %s)", k, val, got, rep.Sources[k])
		}
		tried++
	}
	for k := range wantPolicy {
		if _, ok := find(k); !ok {
			t.Errorf("wantPolicy names %q, which is not a config key", k)
		}
	}
	t.Logf("%d keys tried with a loosening value", tried)
}
