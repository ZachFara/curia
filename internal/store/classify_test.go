package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestIsRead(t *testing.T) {
	for _, c := range []struct {
		q    string
		read bool
	}{
		{"SELECT 1", true},
		{"  \n\tselect 1", true},
		{"-- note\nSELECT 1", true},
		{"/* a */ /* b */ SELECT 1", true},
		{"SELECT 1;", true},
		{"SELECT 1; -- done", true},
		{"SELECT ';', 'it''s' FROM t", true},
		{"VALUES (1)", true},
		{"EXPLAIN QUERY PLAN SELECT 1", true},
		{"WITH RECURSIVE ids(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM ids WHERE n < 3) SELECT n FROM ids", true},
		{"WITH a AS (SELECT 1), b AS MATERIALIZED (SELECT 2) SELECT * FROM a, b", true},
		{"with a as (select 'delete') select * from a", true},
		{"/* c */ WITH a AS (SELECT 1) SELECT * FROM a", true},
		{"PRAGMA journal_mode", true},
		{"PRAGMA table_info(sessions)", true},
		{"PRAGMA journal_mode = WAL", false},
		{"PRAGMA journal_mode(WAL)", false},
		{"PRAGMA user_version = 3", false},
		{"PRAGMA wal_checkpoint(TRUNCATE)", false},
		{"PRAGMA optimize", false},
		{"INSERT INTO t VALUES (1)", false},
		{"UPDATE t SET a = 1 RETURNING a", false},
		{"DELETE FROM t", false},
		{"REPLACE INTO t VALUES (1)", false},
		{"WITH a AS (SELECT 1) INSERT INTO t SELECT * FROM a", false},
		{"WITH a AS (SELECT 1) UPDATE t SET x = (SELECT * FROM a)", false},
		{"WITH a AS (SELECT 1) DELETE FROM t WHERE x IN (SELECT * FROM a)", false},
		{"WITH RECURSIVE a(n) AS (SELECT 1) REPLACE INTO t SELECT n FROM a", false},
		{"-- SELECT\nDELETE FROM t", false},
		{"/* SELECT */ DELETE FROM t", false},
		{"SELECT 1; DELETE FROM t", false},
		{"SELECT 1; /* x */ INSERT INTO t VALUES (1)", false},
		{"CREATE TABLE t (a)", false},
		{"BEGIN", false},
		{"", false},
		{"-- only a comment", false},
		{"/* unterminated SELECT 1", false},
		{"SELECT 'unterminated", false},
	} {
		if got := isRead(c.q); got != c.read {
			t.Errorf("isRead(%q) = %v, want %v", c.q, got, c.read)
		}
	}
}

// TestNoStoreReadTakesTheLock reads every SELECT or WITH SQL string of this
// package's source files: one with no write keyword in it must be a read
// for isRead (so no read of the store queues behind writers), one with a
// write keyword must not.
func TestNoStoreReadTakesTheLock(t *testing.T) {
	n := 0
	for _, q := range sqlLiterals(t) {
		// Skip PRAGMAs, and the pieces of a query built by concatenation.
		if strings.EqualFold(firstWord(q), "PRAGMA") || strings.Count(q, "(") != strings.Count(q, ")") {
			continue
		}
		if ws, _ := topLevelWords(q); len(ws) > 0 && ws[0].text == "WITH" && !hasMainStatement(ws) {
			continue // a CTE prefix whose main statement is appended
		}
		n++
		if want := !writeWord.MatchString(q); isRead(q) != want {
			t.Errorf("isRead = %v, want %v for\n%s", !want, want, q)
		}
	}
	if n < 20 {
		t.Fatalf("found only %d SELECT/WITH statements in the package: the scan is broken", n)
	}
}

var writeWord = regexp.MustCompile(`(?i)\b(insert|update|delete|replace|create|drop|alter|pragma)\b`)

func sqlLiterals(t *testing.T) []string {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if s, ok := sqlLiteral(n); ok {
				out = append(out, s)
			}
			return true
		})
	}
	return out
}

// sqlLiteral returns the string n holds when it starts like a statement.
func sqlLiteral(n ast.Node) (string, bool) {
	lit, ok := n.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	switch strings.ToUpper(firstWord(s)) {
	case "SELECT", "WITH", "PRAGMA":
		return s, true
	}
	return "", false
}

func firstWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

func hasMainStatement(ws []word) bool {
	for _, w := range ws {
		switch w.text {
		case "SELECT", "VALUES", "INSERT", "UPDATE", "DELETE", "REPLACE":
			return true
		}
	}
	return false
}
