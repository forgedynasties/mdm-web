package db

import (
	"context"
	"os"
	"testing"
	"time"
)

// Access by levels: the role baselines are seeded once; a re-run (every start, either server
// copy) changes nothing, a baseline removed by hand stays removed, the built-in profiles are
// gone for good, and a grant given twice is one row with the newer level.
func TestAccessLevelsMigration(t *testing.T) {
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
	count := func(q string, args ...any) int {
		var n int
		if err := d.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	eng := count(`SELECT count(*) FROM access_levels WHERE subject_type = 'role' AND subject = 'engineer'`)
	if eng < 2 {
		t.Fatalf("engineer baselines seeded: %d, want at least View everywhere and Operate on Lab", eng)
	}
	if n := count(`SELECT count(*) FROM access_profiles`); n != 0 {
		t.Errorf("built-in access profiles left: %d", n)
	}
	// Remove a baseline, run again: it stays removed and nothing is duplicated.
	if _, err := d.pool.Exec(ctx, `DELETE FROM access_levels WHERE subject = 'viewer' AND subject_type = 'role'`); err != nil {
		t.Fatal(err)
	}
	before := count(`SELECT count(*) FROM access_levels`)
	if err := d.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	if after := count(`SELECT count(*) FROM access_levels`); after != before {
		t.Errorf("a re-run changed the grants: %d → %d", before, after)
	}
	if n := count(`SELECT count(*) FROM access_profiles`); n != 0 {
		t.Errorf("profiles came back on a re-run: %d", n)
	}
	// A person's grant, given twice: one row, the newer level, then removed.
	user := "lvl-" + time.Now().Format("150405.000")
	g := AccessLevel{SubjectType: "user", Subject: user, ScopeType: "state", State: StateLab, Level: "view", CreatedBy: "test"}
	if err := d.SetAccessLevel(ctx, g); err != nil {
		t.Fatal(err)
	}
	g.Level = "manage"
	if err := d.SetAccessLevel(ctx, g); err != nil {
		t.Fatal(err)
	}
	mine, err := d.UserAccessLevels(ctx, user)
	if err != nil || len(mine) != 1 || mine[0].Level != "manage" || mine[0].Devices < 0 {
		t.Fatalf("own grants = %+v (%v), want one Manage on Lab", mine, err)
	}
	both, _ := d.AccessLevelsFor(ctx, "engineer", user)
	if len(both) != eng+1 {
		t.Errorf("role + own grants = %d, want %d", len(both), eng+1)
	}
	if err := d.DeleteAccessLevel(ctx, mine[0].ID); err != nil {
		t.Fatal(err)
	}
	if left, _ := d.UserAccessLevels(ctx, user); len(left) != 0 {
		t.Errorf("grant not removed: %+v", left)
	}
}
