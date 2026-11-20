package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// layerRepo makes a repository root with the given .curia/config.toml and a
// user file, and returns Options for them.
func layerRepo(t *testing.T, user, repo string) Options {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(root, ".curia"), 0o750); err != nil {
		t.Fatal(err)
	}
	if repo != "" {
		if err := os.WriteFile(filepath.Join(root, ".curia", "config.toml"), []byte(repo), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	up := filepath.Join(dir, "user.toml")
	if user != "" {
		if err := os.WriteFile(up, []byte(user), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return Options{UserPath: up, RepoRoot: root}
}

func refused(r Report, key string) bool {
	for _, x := range r.Refused {
		if x.Key == key {
			return true
		}
	}
	return false
}

func TestEveryKeyHasAPolicy(t *testing.T) {
	for _, k := range Keys() {
		if _, ok := keyPolicy[k]; !ok {
			t.Errorf("config key %q has no entry in keyPolicy (policy.go): add one line", k)
		}
	}
	known := map[string]bool{}
	for _, k := range Keys() {
		known[k] = true
	}
	for k := range keyPolicy {
		if !known[k] {
			t.Errorf("keyPolicy names %q, which is not a config key", k)
		}
	}
}

func TestRepoFileEveryForbiddenKey(t *testing.T) {
	o := layerRepo(t, "", `
editor = "/bin/evil"
db_path = "/tmp/x"
default_context_max = 999999999
[routing]
auto_send = true
auto_threshold = 0.1
[routing.aliases]
a = "b"
[review]
approve = "pr"
[ntfy]
url = "https://evil.example"
[serve]
listen = "127.0.0.1:1"
[restore]
max_age = "9999h"
`)
	c, r, err := Resolve(o)
	if err != nil {
		t.Fatal(err)
	}
	d := Default()
	if c.Editor != "" || c.Routing.AutoSend || c.Routing.AutoThreshold != d.Routing.AutoThreshold ||
		len(c.Routing.Aliases) != 0 || c.Review.Approve != d.Review.Approve || c.Ntfy.URL != "" ||
		c.Serve.Listen != "" || c.Restore.MaxAge != d.Restore.MaxAge || c.DefaultContextMax != d.DefaultContextMax || c.DBPath != d.DBPath {
		t.Errorf("a forbidden repository value took effect: %+v", c)
	}
	for _, k := range []string{"editor", "db_path", "routing.auto_send", "routing.auto_threshold", "routing.aliases", "review.approve", "ntfy.url", "serve.listen", "restore.max_age", "default_context_max"} {
		if !refused(r, k) {
			t.Errorf("%s not reported as refused: %+v", k, r.Refused)
		}
	}
}

func TestRepoFileMayTighten(t *testing.T) {
	o := layerRepo(t, "[routing]\nauto_send = true\nauto_threshold = 0.8\n", "[routing]\nauto_send = false\nauto_threshold = 0.95\n")
	c, r, err := Resolve(o)
	if err != nil {
		t.Fatal(err)
	}
	if c.Routing.AutoSend || c.Routing.AutoThreshold != 0.95 {
		t.Errorf("tightening ignored: %+v", c.Routing)
	}
	if r.Sources["routing.auto_send"] != SourceRepo || r.Sources["routing.auto_threshold"] != SourceRepo {
		t.Errorf("sources = %v", r.Sources)
	}
	// Loosening below the user's value is refused; an int for a float is fine.
	c, r, _ = Resolve(layerRepo(t, "[routing]\nauto_threshold = 0.8\n", "[routing]\nauto_threshold = 0.5\n"))
	if c.Routing.AutoThreshold != 0.8 || !refused(r, "routing.auto_threshold") || r.Sources["routing.auto_threshold"] != SourceUser {
		t.Errorf("loosening took effect: %v %+v", c.Routing, r)
	}
}

func TestLayerOrderAndFlags(t *testing.T) {
	o := layerRepo(t, "stale_hide_after = \"20m\"\n", "")
	c, r, err := Resolve(o)
	if err != nil || c.StaleHideAfter != 20*time.Minute || r.Sources["stale_hide_after"] != SourceUser || r.Sources["assume_expiry"] != SourceDefault {
		t.Fatalf("user layer: %v %v %v", c.StaleHideAfter, r.Sources, err)
	}
	o = layerRepo(t, "[routing]\nauto_threshold = 0.8\n", "[routing]\nauto_threshold = 0.95\n")
	o.Flags = map[string]string{"routing.auto_threshold": "0.5", "routing.auto_send": "true"}
	c, r, err = Resolve(o)
	if err != nil || c.Routing.AutoThreshold != 0.5 || !c.Routing.AutoSend || r.Sources["routing.auto_threshold"] != SourceFlag {
		t.Errorf("flag did not win: %+v %v %v", c.Routing, r.Sources, err)
	}
	o.Flags = map[string]string{"no.such": "1"}
	if _, _, err = Resolve(o); err == nil {
		t.Error("an unknown flag key was accepted")
	}
}

func TestHostileRepoFiles(t *testing.T) {
	cases := map[string]string{
		"invalid TOML": "this is = = not toml",
		"oversized":    "[routing]\nauto_threshold = 0.99\n# " + strings.Repeat("x", maxRepoFile),
		"wrong type":   "[routing]\nauto_threshold = \"high\"\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c, r, err := Resolve(layerRepo(t, "", body))
			if err != nil {
				t.Fatalf("a bad repository file failed the load: %v", err)
			}
			if c.Routing.AutoThreshold != 0.9 {
				t.Errorf("value took effect: %v", c.Routing.AutoThreshold)
			}
			if len(r.Problems)+len(r.Refused) == 0 {
				t.Error("nothing reported")
			}
		})
	}
	t.Run("unknown keys ignored", func(t *testing.T) {
		c, r, err := Resolve(layerRepo(t, "", "future_key = 1\n[routing]\nauto_threshold = 0.95\n"))
		if err != nil || c.Routing.AutoThreshold != 0.95 || len(r.Problems) != 1 {
			t.Errorf("%v %v %+v", err, c.Routing, r.Problems)
		}
	})
	t.Run("symlink outside the repository", func(t *testing.T) {
		o := layerRepo(t, "", "")
		outside := filepath.Join(t.TempDir(), "evil.toml")
		if err := os.WriteFile(outside, []byte("[routing]\nauto_threshold = 0.99\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(o.RepoRoot, ".curia", "config.toml")); err != nil {
			t.Fatal(err)
		}
		c, r, err := Resolve(o)
		if err != nil || c.Routing.AutoThreshold != 0.9 || len(r.Problems) == 0 {
			t.Errorf("symlinked file was read: %v %v %v", err, c.Routing, r.Problems)
		}
	})
}

func TestUserFileUnchangedWithoutRepoFile(t *testing.T) {
	o := layerRepo(t, "notify_bell = true\n", "")
	c, r, err := Resolve(o)
	if err != nil || !c.NotifyBell || len(r.Refused)+len(r.Problems) != 0 {
		t.Errorf("%v %v %+v", err, c.NotifyBell, r)
	}
	if _, _, err = Resolve(layerRepo(t, "assume_expiry = \"0s\"\n", "")); err == nil {
		t.Error("an invalid user value was accepted")
	}
}

func TestUpdateFile(t *testing.T) {
	data, err := UpdateFile([]byte("notify_bell = true\n"), "routing.auto_threshold", "0.7")
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := FileValue(data, "routing.auto_threshold"); v != "0.7" {
		t.Errorf("value = %q in %s", v, data)
	}
	if v, _ := FileValue(data, "notify_bell"); v != "true" {
		t.Errorf("other key lost: %s", data)
	}
	for key, val := range map[string]string{"routing.auto_threshold": "7", "no.such": "1", "routing.aliases": "x", "assume_expiry": "soon"} {
		if _, err = UpdateFile(nil, key, val); err == nil {
			t.Errorf("set %s=%s was accepted", key, val)
		}
	}
}
