package db

import (
	"context"
	"os"
	"testing"
)

// TestReleasesWithoutQAAgainstPostgres runs the release and problem queries that lost
// their QA columns on 21 Oct 2026: reading a release, the list, filing and listing
// problems (including an old QA-sourced one), a branch merging without a "testing done"
// mark, and a user's stats.
func TestReleasesWithoutQAAgainstPostgres(t *testing.T) {
	url := os.Getenv("MDM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MDM_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	d, err := New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	var main int
	if err := d.pool.QueryRow(ctx, `INSERT INTO releases (version, product, status) VALUES ('noqa-1', 't7', 'published') RETURNING id`).Scan(&main); err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM releases WHERE version LIKE 'noqa-%'`)

	rel, err := d.GetRelease(ctx, main)
	if err != nil || rel.Version != "noqa-1" {
		t.Fatalf("GetRelease = %+v, %v", rel, err)
	}
	all, err := d.ListReleases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range all {
		found = found || r.ID == main
	}
	if !found {
		t.Error("ListReleases is missing the release")
	}

	if _, err := d.CreateReleaseProblem(ctx, ReleaseProblem{ReleaseID: main, BuildID: "noqa-1", Title: "noqa crash", Severity: "blocker", ReportedBy: "noqa-user"}); err != nil {
		t.Fatal(err)
	}
	// A problem the old QA checklist filed still lists.
	if _, err := d.pool.Exec(ctx, `INSERT INTO release_problems (release_id, build_id, title, severity, status, source, reported_by) VALUES ($1, 'noqa-1', 'QA fail: boots', 'major', 'open', 'qa', 'noqa-user')`, main); err != nil {
		t.Fatal(err)
	}
	ps, err := d.ListReleaseProblems(ctx, main)
	if err != nil || len(ps) != 2 {
		t.Fatalf("ListReleaseProblems = %d, %v", len(ps), err)
	}
	if _, err := d.CarriedForwardProblems(ctx, main); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ListProblemBoard(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.VerifyProblem(ctx, ps[0].ID, main); err != nil {
		t.Fatal(err)
	}

	branch, err := d.CreateBranchRelease(ctx, main, "noqa-1-t1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	merged, err := d.MergeBranch(ctx, branch, "noqa-1-t1", "noqa-2", "", "", "noqa-user", false)
	if err != nil {
		t.Fatalf("a branch merges with no testing-done mark: %v", err)
	}
	if got, _ := d.GetRelease(ctx, merged); got == nil || got.MergedFromReleaseID == nil || *got.MergedFromReleaseID != branch {
		t.Errorf("merged release = %+v", got)
	}
	if _, err := d.MergeBranch(ctx, branch, "noqa-1-t1", "noqa-3", "", "", "noqa-user", false); err == nil {
		t.Error("a branch merged twice")
	}

	if st, err := d.UserStats(ctx, "noqa-user", ""); err != nil || st.ReleasesMerged != 1 {
		t.Errorf("UserStats = %+v, %v", st, err)
	}
}
