package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/spf13/cobra"
)

// jsonOwnEncoder lists the files that may call json.NewEncoder themselves:
// the helper, and files other work is editing that already escape by hand.
var jsonOwnEncoder = map[string]string{
	"jsonout.go": "the helper",
}

// Every command prints JSON through writeJSON (D-4): no source file but the
// helper builds a json.Encoder, so a new --json path cannot skip the escaping.
func TestEveryJSONPrinterUsesWriteJSON(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := fs.ReadFile(os.DirFS("."), f)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := jsonOwnEncoder[f]; !ok && strings.Contains(string(b), "json.NewEncoder(") {
			t.Errorf("%s encodes JSON itself: print through writeJSON", f)
		}
	}
}

// jsonFixtures: a command with a --json flag and a cheap way to hold a hostile
// value, or the reason it has none here. A new --json command fails the test
// until it is listed.
var jsonFixtures = map[string]func(t *testing.T) []string{
	"curia ls": func(t *testing.T) []string {
		fleetHome(t, []string{"%1"}, model.Session{
			ID: "x", Agent: "claude", Pane: "%1", State: model.StateWaiting, UpdatedAt: at(11, 0),
			Repo: "repo\x1b]0;owned\x07", Branch: "br\u202eanch\u009b2J", LastUpdate: "x\u2066y\U000E0001",
		})
		return []string{"ls", "--json"}
	},
	"curia task ls": func(t *testing.T) []string {
		db := taskHome(t)
		taskFiledOne(t, db, taskRepo(t, "main"))
		return []string{"task", "ls", "--json"}
	},
	"curia schedule ls": func(t *testing.T) []string {
		db := schedHome(t)
		st, _ := store.Open(db)
		defer func() { _ = st.Close() }()
		sc := store.Schedule{Text: "go\x1b]0;owned\x07 \u202eevil\u2066x", Kind: "at", CreatedVia: "cli\u202e\x1b[2J", Spec: "2026-11-12T02:00:00Z", Target: "all", NextRun: at(12, 0), CreatedAt: at(9, 0)}
		if _, err := st.CreateSchedule(context.Background(), sc, 50); err != nil {
			t.Fatal(err)
		}
		return []string{"schedule", "ls", "--json"}
	},
}

// jsonNoFixture are the --json commands whose hostile fixture needs a store
// of several kinds of rows or a live source; each is covered by its own test.
var jsonNoFixture = map[string]string{
	"curia activity":      "needs a transcript and hook-event history",
	"curia decide":        "needs an open ticket and a delivery",
	"curia decisions ls":  "own test: TestDecisionsLsJSON* with decisionsHostile",
	"curia inbox":         "needs a ticket, a review and a message together",
	"curia messages ls":   "needs queued messages",
	"curia resources":     "needs a monitor sample",
	"curia review":        "needs a git repository with a branch under review",
	"curia reviews ls":    "own test: reviews_test.go hostile fixture",
	"curia routing stats": "numbers only",
	"curia show":          "needs a transcript file",
	"curia timeline":      "needs hook events",
	"curia usage":         "numbers and cleaned ids only",
	"curia export":        "needs a repository and a task history",
	"curia time":          "needs hook-event history (same data as curia activity)",
}

func TestEveryJSONCommandIsCovered(t *testing.T) {
	var paths []string
	userOnlyWalk(rootCmd, func(c *cobra.Command) {
		if c.Flags().Lookup("json") != nil {
			paths = append(paths, c.CommandPath())
		}
	})
	if len(paths) == 0 {
		t.Fatal("found no command with a --json flag")
	}
	for _, p := range paths {
		_, fix := jsonFixtures[p]
		_, no := jsonNoFixture[p]
		if fix == no {
			t.Errorf("%s has a --json flag: list it in jsonFixtures (a hostile value) or jsonNoFixture (why not), not both or neither", p)
		}
	}
}

func TestJSONOutputHoldsNoRawUnprintableRune(t *testing.T) {
	for p, setup := range jsonFixtures {
		t.Run(p, func(t *testing.T) {
			argv := setup(t)
			out, err := runRoot(t, argv...)
			if err != nil {
				t.Fatalf("%v: %v\n%s", argv, err, out)
			}
			if !strings.Contains(out, `\u202e`) && !strings.Contains(out, `\u2066`) && !strings.Contains(out, `\u001b`) {
				t.Errorf("%v: the hostile value is not in the output:\n%s", argv, out)
			}
			for _, r := range out {
				if r != '\n' && !unicode.IsPrint(r) {
					t.Errorf("%v printed the raw rune %U:\n%s", argv, r, out)
				}
			}
		})
	}
}
