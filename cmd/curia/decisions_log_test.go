package main

import (
	"strings"
	"testing"
)

func TestDecisionsLogCommand(t *testing.T) {
	timelineSeed(t)
	raw := rawSQLite(t, mustEnv(t, "CURIA_DB"))
	for _, q := range []string{
		`INSERT INTO decisions (id, question, options, recommendation, rationale, reversibility, mode, scope, status, answer, args_hash, created_at, answered_at)
VALUES ('D-1', 'Which database for webhook events?', '[]', 'a', 'r', 'cheap', 'blocking', 'repo', 'answered', 'B: Redis', 'h1', '2026-11-19T09:00:00.000000000Z', '2026-11-19T09:05:00.000000000Z')`,
		`INSERT INTO decisions (id, question, options, recommendation, rationale, reversibility, mode, scope, status, args_hash, precedent_of, created_at, closed_at)
VALUES ('D-2', 'cites one', '[]', 'a', 'r', 'cheap', 'blocking', 'repo', 'withdrawn', 'h2', 'D-1', '2026-11-19T10:00:00.000000000Z', '2026-11-19T10:01:00.000000000Z')`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { decisionlogRepo, decisionlogScope, decisionlogStatus, decisionlogSince = "", "", "", "" })
	out, err := runRoot(t, "decisions", "log", "webhook")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "D-1") || !strings.Contains(out, "1 (last") || strings.Contains(out, "D-2") ||
		!strings.Contains(out, "1 of 1 answered decisions (100%) were reused at least once") {
		t.Errorf("log output:\n%s", out)
	}
}
