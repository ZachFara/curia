package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/install"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
)

// configMaxFile bounds the user file an editor leaves, in bytes.
const configMaxFile = 1 << 20

// configHistoryN is the flag of `curia config history`; tests reset it.
var configHistoryN int

func init() {
	parent := &cobra.Command{Use: "config", Short: "Show and change the settings (layered, with history)"}
	list := &cobra.Command{Use: "list", Short: "Every key, its effective value and the layer it came from", Args: cobra.NoArgs, RunE: runConfigList}
	get := &cobra.Command{Use: "get <key>", Short: "One key's effective value and layer", Args: cobra.ExactArgs(1), RunE: runConfigGet}
	set := &cobra.Command{
		Use: "set <key> <value>", Short: "Set a key in the user file (the user only)", Annotations: userOnlyMark(),
		Args: cobra.ExactArgs(2), RunE: userOnlyRunE("config set", "change settings", runConfigSet),
	}
	edit := &cobra.Command{
		Use: "edit", Short: "Edit the user file with the `editor` option (the user only)", Annotations: userOnlyMark(),
		Args: cobra.NoArgs, RunE: userOnlyRunE("config edit", "change settings", runConfigEdit),
	}
	doctor := &cobra.Command{
		Use: "doctor", Short: "Check both files; at a terminal, acknowledge an external edit",
		Args: cobra.NoArgs, RunE: runConfigDoctor,
	}
	history := &cobra.Command{Use: "history", Short: "List the revisions of the user file", Args: cobra.NoArgs, RunE: runConfigHistory}
	history.Flags().IntVarP(&configHistoryN, "number", "n", 20, "how many revisions to show")
	rollback := &cobra.Command{
		Use: "rollback <n>", Short: "Restore the file as it was after revision n, as a new revision (the user only)",
		Annotations: userOnlyMark(), Args: cobra.ExactArgs(1),
		RunE: userOnlyRunE("config rollback", "change settings", runConfigRollback),
	}
	parent.AddCommand(list, get, set, edit, doctor, history, rollback)
	rootCmd.AddCommand(parent)
}

func configUserPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find config: %w", err)
	}
	return configFile(home), nil
}

func configResolve(path string) (config.Config, config.Report, error) {
	wd, _ := os.Getwd()
	return config.Resolve(config.Options{UserPath: path, RepoRoot: config.FindRepoRoot(wd)})
}

func configReadUser(path string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return b, nil
}

// configSum is a short hash of file text, for revisions that hold a whole file.
func configSum(b string) string {
	h := sha256.Sum256([]byte(b))
	return "sha256:" + hex.EncodeToString(h[:6])
}

// configSync records an edit made outside Curia (the file's hash differs
// from the newest revision's file) as an `external` revision. With no
// revision yet the file is the baseline.
func configSync(ctx context.Context, st *store.Store, path string) error {
	revs, err := st.ConfigRevisions(ctx, 1)
	if err != nil || len(revs) == 0 {
		return err
	}
	cur, err := configReadUser(path)
	if err != nil {
		return err
	}
	if string(cur) == revs[0].File {
		return nil
	}
	_, err = st.AddConfigRevision(ctx, store.ConfigRevision{
		At: time.Now(), Actor: "external", Key: "*", Old: configSum(revs[0].File), New: configSum(string(cur)), File: string(cur),
	})
	return err
}

// configOpen opens the store and syncs; a failure to sync is returned only
// when strict (the commands that write).
func configOpen(ctx context.Context, path string, strict bool) (*store.Store, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	st, err := openStore(cfg)
	if err != nil {
		return nil, err
	}
	if err = configSync(ctx, st, path); err != nil && strict {
		_ = st.Close()
		return nil, err
	}
	return st, nil
}

func configLine(c config.Config, rep config.Report, key string) string {
	v, _ := config.Get(c, key)
	return fmt.Sprintf("%s = %s  (%s)", key, sanitize.Line(v, 200), rep.Sources[key])
}

func configPrintRefused(w io.Writer, rep config.Report, only string) {
	for _, r := range rep.Refused {
		if only == "" || only == r.Key {
			_, _ = fmt.Fprintf(w, "repository value refused: %s = %s: %s\n", r.Key, sanitize.Line(r.Value, 100), sanitize.Line(r.Reason, 200))
		}
	}
}

func runConfigList(cmd *cobra.Command, _ []string) error {
	path, err := configUserPath()
	if err != nil {
		return err
	}
	if st, oerr := configOpen(cmd.Context(), path, false); oerr == nil {
		_ = st.Close()
	}
	c, rep, err := configResolve(path)
	if err != nil {
		return err
	}
	for _, r := range settingsRows(c, rep) {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s = %s  (%s)\n", r.Key, r.Value, r.Source)
	}
	configPrintRefused(cmd.OutOrStdout(), rep, "")
	return nil
}

func runConfigGet(cmd *cobra.Command, args []string) error {
	path, err := configUserPath()
	if err != nil {
		return err
	}
	c, rep, err := configResolve(path)
	if err != nil {
		return err
	}
	if _, ok := config.Get(c, args[0]); !ok {
		return fmt.Errorf("unknown config key %q (see `curia config list`)", sanitize.Line(args[0], 80))
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), configLine(c, rep, args[0]))
	configPrintRefused(cmd.OutOrStdout(), rep, args[0])
	return nil
}

// configWrite validates data, replaces the user file atomically (the old
// file stays when anything fails) and appends the revision.
func configWrite(ctx context.Context, st *store.Store, path string, data []byte, rev store.ConfigRevision) (int64, error) {
	if err := config.ValidateUser(data); err != nil {
		return 0, fmt.Errorf("not written, the old file is kept: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, err
	}
	if err := install.WriteAtomic(path, data); err != nil {
		return 0, err
	}
	rev.At, rev.File = time.Now(), string(data)
	return st.AddConfigRevision(ctx, rev)
}

func runConfigSet(cmd *cobra.Command, args []string) error {
	path, err := configUserPath()
	if err != nil {
		return err
	}
	st, err := configOpen(cmd.Context(), path, true)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	nv, n, err := configSetKey(cmd.Context(), st, path, args[0], args[1], "cli")
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s = %s (revision %d)\n", args[0], sanitize.Line(nv, 200), n)
	return nil
}

// configSetKey is the one write of a scalar key, for `config set` and the
// dashboard: validated, the old file kept when invalid, one revision.
func configSetKey(ctx context.Context, st *store.Store, path, key, value, actor string) (string, int64, error) {
	cur, err := configReadUser(path)
	if err != nil {
		return "", 0, err
	}
	data, err := config.UpdateFile(cur, key, value)
	if err != nil {
		return "", 0, err
	}
	old, _ := config.FileValue(cur, key)
	nv, _ := config.FileValue(data, key)
	n, err := configWrite(ctx, st, path, data, store.ConfigRevision{Actor: actor, Key: key, Old: old, New: nv})
	return nv, n, err
}

func runConfigEdit(cmd *cobra.Command, _ []string) error {
	path, err := configUserPath()
	if err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if len(cfg.EditorArgs()) == 0 {
		return fmt.Errorf("no editor set: add editor = \"/path/to/editor\" to %s (curia never reads $EDITOR)", path)
	}
	st, err := configOpen(cmd.Context(), path, true)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	cur, err := configReadUser(path)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "curia-config-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	tmp := filepath.Join(dir, "config.toml")
	if err = os.WriteFile(tmp, cur, 0o600); err != nil {
		return err
	}
	if err = taskEditor(cmd.Context(), cfg.EditorArgs(), tmp); err != nil {
		return fmt.Errorf("editor: %w", err)
	}
	data, err := configReadEdited(tmp)
	if err != nil {
		return err
	}
	if string(data) == string(cur) {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no changes")
		return nil
	}
	n, err := configWrite(cmd.Context(), st, path, data, store.ConfigRevision{Actor: "cli", Key: "*", Old: configSum(string(cur)), New: configSum(string(data))})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "saved (revision %d)\n", n)
	return nil
}

// configReadEdited reads the file the editor left, bounded.
func configReadEdited(tmp string) ([]byte, error) {
	f, err := os.Open(filepath.Clean(tmp))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, configMaxFile+1))
	if err != nil || len(data) > configMaxFile {
		return nil, fmt.Errorf("the edited file is unreadable or over %d bytes: the old file is kept", configMaxFile)
	}
	return data, nil
}

func runConfigHistory(cmd *cobra.Command, _ []string) error {
	path, err := configUserPath()
	if err != nil {
		return err
	}
	st, err := configOpen(cmd.Context(), path, false)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	revs, err := st.ConfigRevisions(cmd.Context(), max(configHistoryN, 1))
	if err != nil {
		return err
	}
	for _, r := range revs {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%d  %s  %s  %s: %s -> %s\n", r.N, r.At.Local().Format("2006-01-02 15:04:05"),
			sanitize.Line(r.Actor, 20), sanitize.Line(r.Key, 80), sanitize.Line(r.Old, 80), sanitize.Line(r.New, 80))
	}
	return nil
}

func runConfigRollback(cmd *cobra.Command, args []string) error {
	n, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || n < 1 {
		return fmt.Errorf("revision must be a number from 1, got %q", sanitize.Line(args[0], 40))
	}
	path, err := configUserPath()
	if err != nil {
		return err
	}
	st, err := configOpen(cmd.Context(), path, true)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	rev, ok, err := st.ConfigRevision(cmd.Context(), n)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no revision %d (see `curia config history`)", n)
	}
	cur, err := configReadUser(path)
	if err != nil {
		return err
	}
	if err = config.ValidateUser([]byte(rev.File)); err != nil {
		return fmt.Errorf("revision %d is no longer valid, nothing written: %w", n, err)
	}
	answer, err := setupConfirm(fmt.Sprintf("Write the file as it was after revision %d (%s, %s)? Type yes: ", n, rev.At.Local().Format("2006-01-02 15:04"), sanitize.Line(rev.Actor, 20)))
	if err != nil || answer != "yes" {
		return errors.New("not rolled back: confirmation was not given")
	}
	nn, err := configWrite(cmd.Context(), st, path, []byte(rev.File), store.ConfigRevision{
		Actor: "rollback", Key: "*", Old: configSum(string(cur)), New: configSum(rev.File),
	})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "restored revision %d as revision %d\n", n, nn)
	return nil
}

func runConfigDoctor(cmd *cobra.Command, _ []string) error {
	out := cmd.OutOrStdout()
	path, err := configUserPath()
	if err != nil {
		return err
	}
	problems := 0
	say := func(format string, a ...any) {
		problems++
		_, _ = fmt.Fprintf(out, format+"\n", a...)
	}
	st, serr := configOpen(cmd.Context(), path, false)
	if serr != nil {
		say("store: %s", sanitize.Line(serr.Error(), 200))
	} else {
		defer func() { _ = st.Close() }()
	}
	if c, rep, err := configResolve(path); err != nil {
		say("invalid: %s", sanitize.Line(err.Error(), 300))
	} else {
		for _, r := range rep.Refused {
			say("repository value refused: %s = %s: %s", r.Key, sanitize.Line(r.Value, 100), sanitize.Line(r.Reason, 200))
		}
		for _, p := range rep.Problems {
			say("%s", sanitize.Line(p, 300))
		}
		wd, _ := os.Getwd()
		_, rp := activityRepoRules(c.Activity.Rules, config.FindRepoRoot(wd))
		for _, p := range rp {
			if slices.Contains(rep.Problems, p) {
				continue
			}
			say("%s", sanitize.Line(p, 300))
		}
	}
	if st != nil {
		if err := configDoctorExternal(cmd, st, say); err != nil {
			return err
		}
	}
	if problems == 0 {
		_, _ = fmt.Fprintln(out, "ok")
	}
	return nil
}

// configDoctorExternal reports the newest external revision, and
// acknowledges it only when the user guard passes (a person at a terminal).
func configDoctorExternal(cmd *cobra.Command, st *store.Store, say func(string, ...any)) error {
	revs, err := st.ConfigRevisions(cmd.Context(), 50)
	if err != nil {
		return err
	}
	var newest int64
	for _, r := range revs {
		if r.Actor == "external" {
			newest = r.N
			break
		}
	}
	ack, err := st.ConfigAck(cmd.Context())
	if err != nil {
		return err
	}
	if newest == 0 || newest <= ack {
		return nil
	}
	say("the user file was edited outside Curia (revision %d, recorded as external)", newest)
	if userOnly("config doctor", "acknowledge an external edit") == nil {
		if err = st.AckConfig(cmd.Context(), newest); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "acknowledged")
		return nil
	}
	say("run `curia config doctor` at a terminal to acknowledge it")
	return nil
}
