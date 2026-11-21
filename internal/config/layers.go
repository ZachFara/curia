package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Source is the layer a key's effective value came from.
type Source string

// The layers, lowest first.
const (
	SourceDefault Source = "default"
	SourceUser    Source = "user"
	SourceRepo    Source = "repo"
	SourceFlag    Source = "flag"
)

// maxRepoFile caps the repository file, in bytes (Ruling 156).
const maxRepoFile = 64 << 10

// Options are the inputs of Resolve.
type Options struct {
	UserPath string            // the user file; missing is fine
	RepoRoot string            // a repository root, or "" for no repository layer
	Flags    map[string]string // dotted key -> value; the user's own, so unrestricted
}

// Refusal is a repository value that was ignored, and why.
type Refusal struct{ Key, Value, Reason string }

// Report says where each key came from and what the repository file got
// wrong: Resolve never fails because of the repository file.
type Report struct {
	Sources  map[string]Source // every key
	Refused  []Refusal         // repository values that were ignored
	Problems []string          // the repository file unread, or unknown keys in it
}

type leaf struct {
	key   string
	index []int
}

var durationType = reflect.TypeOf(time.Duration(0))

// leaves lists every key of Config: a toml-tagged field that is not a
// struct (a map or list is one key).
func leaves() []leaf {
	var out []leaf
	var walk func(t reflect.Type, prefix string, idx []int)
	walk = func(t reflect.Type, prefix string, idx []int) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := strings.Split(f.Tag.Get("toml"), ",")[0]
			if name == "" || name == "-" {
				continue
			}
			at := append(append([]int{}, idx...), i)
			if f.Type.Kind() == reflect.Struct {
				walk(f.Type, prefix+name+".", at)
				continue
			}
			out = append(out, leaf{prefix + name, at})
		}
	}
	walk(reflect.TypeOf(Config{}), "", nil)
	return out
}

// Keys returns every config key, sorted.
func Keys() []string {
	var ks []string
	for _, l := range leaves() {
		ks = append(ks, l.key)
	}
	sort.Strings(ks)
	return ks
}

func find(key string) (leaf, bool) {
	for _, l := range leaves() {
		if l.key == key {
			return l, true
		}
	}
	return leaf{}, false
}

// Get returns the key's value as text; a table or list shows only its size
// (topics and aliases are not printed).
func Get(c Config, key string) (string, bool) {
	l, ok := find(key)
	if !ok {
		return "", false
	}
	v := reflect.ValueOf(c).FieldByIndex(l.index)
	switch v.Kind() {
	case reflect.Map, reflect.Slice:
		return fmt.Sprintf("(%d entries)", v.Len()), true
	}
	if v.CanInt() && v.Int() == 0 && zeroIsOff[l.key] {
		return "off", true
	}
	return fmt.Sprint(v.Interface()), true
}

// parseValue reads s as the key's type, in the form TOML holds it (a
// duration stays text).
func parseValue(l leaf, s string) (any, error) {
	t := reflect.TypeOf(Config{}).FieldByIndex(l.index).Type
	switch {
	case t == durationType:
		if _, err := time.ParseDuration(s); err != nil {
			return nil, fmt.Errorf("%s must be a duration such as 10m: %w", l.key, err)
		}
		return s, nil
	case t.Kind() == reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return nil, fmt.Errorf("%s must be true or false", l.key)
		}
		return b, nil
	case t.Kind() == reflect.String:
		return s, nil
	case t.Kind() == reflect.Int:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s must be a whole number", l.key)
		}
		return n, nil
	case t.Kind() == reflect.Float64:
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, fmt.Errorf("%s must be a number", l.key)
		}
		return f, nil
	}
	return nil, fmt.Errorf("%s is a table or list: edit the file with `curia config edit`", l.key)
}

// assign stores a TOML-typed value v in the key's field, checking its type.
func assign(c *Config, l leaf, v any) error {
	f := reflect.ValueOf(c).Elem().FieldByIndex(l.index)
	bad := fmt.Errorf("%s: wrong type %T", l.key, v)
	switch {
	case f.Type() == durationType:
		s, ok := v.(string)
		if !ok {
			return bad
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("%s: %w", l.key, err)
		}
		f.SetInt(int64(d))
	case f.Kind() == reflect.Bool:
		b, ok := v.(bool)
		if !ok {
			return bad
		}
		f.SetBool(b)
	case f.Kind() == reflect.String:
		s, ok := v.(string)
		if !ok {
			return bad
		}
		f.SetString(s)
	case f.Kind() == reflect.Int:
		n, ok := v.(int64)
		if !ok {
			return bad
		}
		f.SetInt(n)
	case f.Kind() == reflect.Float64:
		switch n := v.(type) {
		case float64:
			f.SetFloat(n)
		case int64:
			f.SetFloat(float64(n))
		default:
			return bad
		}
	default:
		return fmt.Errorf("%s: a table or list cannot be set this way", l.key)
	}
	return nil
}

// number is a numeric or duration field as a float, for direction checks.
func number(c Config, l leaf) float64 {
	v := reflect.ValueOf(c).FieldByIndex(l.index)
	if v.CanFloat() {
		return v.Float()
	}
	return float64(v.Int())
}

// zeroIsOff are the keys whose 0 means off (or no limit): shown as "off", and
// for a repository file 0 is the loosest value.
var zeroIsOff = map[string]bool{
	"resources.max_containers_per_session": true, "resources.max_containers_total": true,
	"resources.docker_memory_refuse_pct": true, "resources.container_memory_mb": true, "resources.container_cpus": true,
}

// loosens reports whether going from old to next breaks the policy.
func loosens(p Policy, l leaf, old, next Config) bool {
	if l.key == "cost.task_budget_default" || zeroIsOff[l.key] {
		// 0 means none or off, so it is not "lower": a repository may only lower a nonzero value, never switch it off or set one over none.
		o, n := number(old, l), number(next, l)
		return (o == 0) != (n == 0) || n > o
	}
	switch p {
	case PolicyLower:
		return number(next, l) > number(old, l)
	case PolicyRaise:
		return number(next, l) < number(old, l)
	case PolicyFalse:
		return reflect.ValueOf(next).FieldByIndex(l.index).Bool()
	case PolicyTrue:
		return !reflect.ValueOf(next).FieldByIndex(l.index).Bool()
	case PolicyLevel:
		return levelRank[reflect.ValueOf(next).FieldByIndex(l.index).String()] > levelRank[reflect.ValueOf(old).FieldByIndex(l.index).String()]
	}
	return true
}

// FindRepoRoot returns the nearest directory at or above dir holding a
// .git (a directory, or a linked worktree's file), or "".
func FindRepoRoot(dir string) string {
	for d := dir; d != "" && d != string(filepath.Separator) && d != filepath.Dir(d); d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d
		}
	}
	return ""
}

// Resolve layers defaults < the user file < the repository's
// .curia/config.toml < flags and validates the result. The repository file
// is untrusted: see policy.go. An invalid user file or flag is an error; a
// bad repository file is only reported.
func Resolve(o Options) (Config, Report, error) {
	c := Default()
	rep := Report{Sources: map[string]Source{}}
	for _, l := range leaves() {
		rep.Sources[l.key] = SourceDefault
	}
	data, err := os.ReadFile(filepath.Clean(o.UserPath))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return Config{}, rep, fmt.Errorf("read config: %w", err)
	default:
		if err := toml.Unmarshal(data, &c); err != nil {
			return Config{}, rep, fmt.Errorf("parse config %s: %w", o.UserPath, err)
		}
		var raw map[string]any
		_ = toml.Unmarshal(data, &raw)
		for _, l := range leaves() {
			if _, ok := dig(raw, l.key); ok {
				rep.Sources[l.key] = SourceUser
			}
		}
	}
	if err := c.validate(); err != nil {
		return Config{}, rep, fmt.Errorf("config %s: %w", o.UserPath, err)
	}
	if o.RepoRoot != "" {
		c = applyRepo(c, o.RepoRoot, &rep)
	}
	for _, k := range sortedKeys(o.Flags) {
		l, ok := find(k)
		if !ok {
			return Config{}, rep, fmt.Errorf("unknown config key %q", k)
		}
		v, err := parseValue(l, o.Flags[k])
		if err == nil {
			err = assign(&c, l, v)
		}
		if err != nil {
			return Config{}, rep, err
		}
		rep.Sources[k] = SourceFlag
	}
	if err := c.validate(); err != nil {
		return Config{}, rep, fmt.Errorf("config: %w", err)
	}
	c.DBPath = expandHome(c.DBPath)
	c.LogPath = expandHome(c.LogPath)
	if env := os.Getenv("CURIA_DB"); env != "" {
		c.DBPath = env
	}
	return c, rep, nil
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// dig finds a dotted key in a decoded TOML tree.
func dig(m map[string]any, key string) (any, bool) {
	var v any = m
	for _, p := range strings.Split(key, ".") {
		t, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if v, ok = t[p]; !ok {
			return nil, false
		}
	}
	return v, true
}

// readRepoFile returns the repository file's bytes, or why it was not read.
// shortcut: the symlink check and the open are not atomic, so a swap between
// them is possible; a same-user attacker can edit the user file anyway.
func readRepoFile(root string) ([]byte, string) {
	p := filepath.Join(root, ".curia", "config.toml")
	if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
		return nil, ""
	}
	realRoot, err1 := filepath.EvalSymlinks(root)
	resolved, err2 := filepath.EvalSymlinks(p)
	if err1 != nil || err2 != nil || resolved != filepath.Join(realRoot, ".curia", "config.toml") {
		return nil, "repository config is a symlink or under one, or unreadable: ignored"
	}
	f, err := os.Open(resolved)
	if err != nil {
		return nil, "repository config unreadable: ignored"
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxRepoFile+1))
	switch {
	case err != nil:
		return nil, "repository config unreadable: ignored"
	case len(b) > maxRepoFile:
		return nil, fmt.Sprintf("repository config is over %d bytes: ignored", maxRepoFile)
	}
	return b, ""
}

// applyRepo applies the repository file's keys through keyPolicy, one at a
// time, each only if the result still validates.
func applyRepo(c Config, root string, rep *Report) Config {
	data, problem := readRepoFile(root)
	if problem != "" {
		rep.Problems = append(rep.Problems, problem)
	}
	if data == nil {
		return c
	}
	var raw map[string]any
	if err := toml.Unmarshal(data, &raw); err != nil {
		rep.Problems = append(rep.Problems, "repository config is not valid TOML: ignored")
		return c
	}
	var walk func(m map[string]any, prefix string)
	walk = func(m map[string]any, prefix string) {
		names := make([]string, 0, len(m))
		for n := range m {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			key, v := prefix+n, m[n]
			l, known := find(key)
			if sub, ok := v.(map[string]any); ok && !known {
				walk(sub, key+".")
				continue
			}
			if !known {
				rep.Problems = append(rep.Problems, "repository config: unknown key "+key+" ignored")
				continue
			}
			c = applyRepoKey(c, l, v, rep)
		}
	}
	walk(raw, "")
	return c
}

func applyRepoKey(c Config, l leaf, v any, rep *Report) Config {
	no := func(why string) Config {
		rep.Refused = append(rep.Refused, Refusal{l.key, fmt.Sprint(v), why})
		return c
	}
	pol := keyPolicy[l.key]
	if pol == PolicyRepoRules {
		return c
	}
	if pol == PolicyNone {
		return no("a repository file cannot change this key")
	}
	next := c
	if err := assign(&next, l, v); err != nil {
		return no(err.Error())
	}
	if loosens(pol, l, c, next) {
		cur, _ := Get(c, l.key)
		return no("it would loosen the setting (now " + cur + "); a repository file may only tighten")
	}
	if err := next.validate(); err != nil {
		return no(err.Error())
	}
	rep.Sources[l.key] = SourceRepo
	return next
}

// ValidateUser checks the bytes of a user file as Load would.
func ValidateUser(data []byte) error {
	c := Default()
	if err := toml.Unmarshal(data, &c); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	return c.validate()
}

// FileValue is the key's value in the user file's bytes (the default when
// the file does not set it).
func FileValue(data []byte, key string) (string, bool) {
	c := Default()
	if toml.Unmarshal(data, &c) != nil {
		return "", false
	}
	return Get(c, key)
}

// UpdateFile returns the user file's bytes with key set to value, checked
// against the key's type and the whole file's rules.
// shortcut: the file is re-encoded, so its comments and layout are lost;
// every change is kept whole in the revision history, so rollback restores
// them. Upgrade if a comment-preserving TOML editor is ever vendored.
func UpdateFile(data []byte, key, value string) ([]byte, error) {
	l, ok := find(key)
	if !ok {
		return nil, fmt.Errorf("unknown config key %q", key)
	}
	v, err := parseValue(l, value)
	if err != nil {
		return nil, err
	}
	raw := map[string]any{}
	if err = toml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	parts := strings.Split(key, ".")
	m := raw
	for _, p := range parts[:len(parts)-1] {
		sub, ok := m[p].(map[string]any)
		if !ok {
			sub = map[string]any{}
			m[p] = sub
		}
		m = sub
	}
	m[parts[len(parts)-1]] = v
	var b strings.Builder
	if err = toml.NewEncoder(&b).Encode(raw); err != nil {
		return nil, err
	}
	if err = ValidateUser([]byte(b.String())); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

// maxRepoRules bounds the rules read from one repository file.
const maxRepoRules = 64

// RepoActivityRules returns the activity.rules of the repository at root's
// .curia/config.toml (untrusted: same size cap, symlink rule and strict TOML
// as the other keys), at most maxRepoRules of them, and what went wrong
// reading them. The caller compiles each and drops the ones that fail.
func RepoActivityRules(root string) ([]ActivityRule, []string) {
	if root == "" {
		return nil, nil
	}
	data, problem := readRepoFile(root)
	if problem != "" {
		return nil, []string{problem}
	}
	if data == nil {
		return nil, nil
	}
	var f struct {
		Activity struct {
			Rules []ActivityRule `toml:"rules"`
		} `toml:"activity"`
	}
	if err := toml.Unmarshal(data, &f); err != nil {
		return nil, []string{"repository config activity.rules not read: " + err.Error()}
	}
	r := f.Activity.Rules
	if len(r) > maxRepoRules {
		return r[:maxRepoRules], []string{fmt.Sprintf("repository config: more than %d activity rules, the rest ignored", maxRepoRules)}
	}
	return r, nil
}
