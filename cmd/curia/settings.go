package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/ui"
)

// settingsNoEdit are the keys the dashboard never changes: they name a program
// or a path, or widen what an agent may do (the spec lists no typed word for a
// key, so these are kept to `curia config set` and `edit`).
var settingsNoEdit = map[string]bool{
	"editor": true, "review.diff_tool": true, "costlens.analyzer": true,
	"db_path": true, "log_path": true, "serve.listen": true, "routing.auto_send": true,
}

// settingsRows is the one data path of `config list` and the dashboard's
// settings view: every key, its text (a table or list only as a count), its
// layer and a repository value that was refused.
func settingsRows(c config.Config, rep config.Report) []ui.SettingRow {
	var rows []ui.SettingRow
	for _, k := range config.Keys() {
		v, _ := config.Get(c, k)
		r := ui.SettingRow{Key: k, Value: sanitize.Line(v, 200), Source: string(rep.Sources[k])}
		r.Editable = !settingsNoEdit[k] && (!strings.HasPrefix(v, "(") || !strings.HasSuffix(v, " entries)"))
		for _, f := range rep.Refused {
			if f.Key == k {
				r.Refused = sanitize.Line(f.Value, 100) + ": " + sanitize.Line(f.Reason, 200)
			}
		}
		rows = append(rows, r)
	}
	return rows
}

// settingsSource serves the dashboard's settings view. Set is `curia config
// set` (configSetKey), refused when the user guard refused the dashboard.
type settingsSource struct {
	st      *store.Store
	refused error
}

func (s settingsSource) Rows(context.Context) ([]ui.SettingRow, error) {
	path, err := configUserPath()
	if err != nil {
		return nil, err
	}
	c, rep, err := configResolve(path)
	if err != nil {
		return nil, err
	}
	return settingsRows(c, rep), nil
}

func (s settingsSource) Set(ctx context.Context, key, value string) (string, error) {
	if s.refused != nil {
		return "", s.refused
	}
	if settingsNoEdit[key] {
		return "", fmt.Errorf("%s is changed with curia config set", sanitize.Line(key, 60))
	}
	path, err := configUserPath()
	if err != nil {
		return "", err
	}
	if err = configSync(ctx, s.st, path); err != nil {
		return "", err
	}
	nv, n, err := configSetKey(ctx, s.st, path, key, value, "dashboard")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s = %s (revision %d)", key, sanitize.Line(nv, 100), n), nil
}

// settingsExternalPending reports an edit of the user file made outside
// Curia that `curia config doctor` has not acknowledged. It runs in every
// `curia status`: one indexed row, a stat, and a read of the file only when
// its size equals the stored file's and is small. Any failure is "no".
func settingsExternalPending(ctx context.Context, st *store.Store) bool {
	h, ok, err := st.ConfigHead(ctx)
	if err != nil || !ok {
		return false
	}
	if h.External > h.Ack {
		return true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	path := configFile(home)
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return len(h.File) > 0
	}
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	if fi.Size() != int64(len(h.File)) {
		return true
	}
	if fi.Size() > 64<<10 {
		return false
	}
	b, err := os.ReadFile(filepath.Clean(path))
	return err == nil && string(b) != h.File
}

// settingsReason is why the view is read-only: the guard's reason, or "".
func settingsReason(refused error) string {
	if refused == nil {
		return ""
	}
	return refused.Error()
}
