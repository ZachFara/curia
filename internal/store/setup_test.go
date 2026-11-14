package store_test

import (
	"context"
	"testing"
)

func TestSetupApprovalPerRepository(t *testing.T) {
	ctx := context.Background()
	s := open(t, dbPath(t))
	get := func(repo string) string {
		t.Helper()
		sum, err := s.SetupApproval(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		return sum
	}
	if got := get("/src/api"); got != "" {
		t.Fatalf("approval before any = %q, want none", got)
	}
	if err := s.ApproveSetup(ctx, "/src/api", "aaaa"); err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveSetup(ctx, "/src/web", "cccc"); err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveSetup(ctx, "/src/api", "bbbb"); err != nil {
		t.Fatal(err)
	}
	if got := get("/src/api"); got != "bbbb" {
		t.Errorf("/src/api approval = %q, want the later bbbb", got)
	}
	if got := get("/src/web"); got != "cccc" {
		t.Errorf("/src/web approval = %q, want cccc", got)
	}
	if got := get("/src/api/"); got != "" {
		t.Errorf("another path's approval = %q, want none", got)
	}
}
