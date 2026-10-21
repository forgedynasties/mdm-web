package db

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestPeopleActivityAgainstPostgres: actions by people per day (page views and actors
// with no account left out of the count), one person's week, and live sessions.
func TestPeopleActivityAgainstPostgres(t *testing.T) {
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
	before, err := d.PeopleActivityDaily(ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	u, err := d.CreateUser(ctx, "pat-alice", "x", "viewer", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM users WHERE username LIKE 'pat-%'`)
	defer d.pool.Exec(ctx, `DELETE FROM audit_log WHERE actor LIKE 'pat-%'`)
	defer d.pool.Exec(ctx, `DELETE FROM sessions WHERE username LIKE 'pat-%'`)
	for _, q := range []string{
		`INSERT INTO audit_log (actor, action, target, detail) VALUES ('pat-alice', 'device.reboot', 'x', ''), ('pat-alice', 'device.reboot', 'y', ''), ('pat-alice', 'page.view', '/', '')`,
		`INSERT INTO audit_log (actor, action, target, detail, created_at) VALUES ('pat-alice', 'device.reboot', 'z', '', NOW() - interval '10 days')`,
		`INSERT INTO audit_log (actor, action, target, detail) VALUES ('pat-ghost', 'device.reboot', 'x', '')`, // no account
	} {
		if _, err := d.pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	days, err := d.PeopleActivityDaily(ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 8 {
		t.Fatalf("days = %d, want 8", len(days))
	}
	if got, was := days[7], before[7]; got.Actions-was.Actions != 2 || got.People-was.People != 1 {
		t.Errorf("today = %+v (before %+v), want +2 actions by +1 person", got, was)
	}

	w, err := d.ActorWeekCounts(ctx, "pat-alice")
	if err != nil {
		t.Fatal(err)
	}
	if w.Actions != 2 || w.PageViews != 1 || w.PrevActions != 1 {
		t.Errorf("week = %+v, want 2 actions, 1 page view, 1 the week before", w)
	}

	if err := d.CreateSession(ctx, Session{ID: "pat-s1", UserID: &u.ID, Username: "pat-alice", Role: "viewer", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := d.CreateSession(ctx, Session{ID: "pat-s2", UserID: &u.ID, Username: "pat-alice", Role: "viewer", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	in, err := d.SignedInNow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range in {
		if s.Username == "pat-alice" {
			found = true
			if s.Sessions != 1 {
				t.Errorf("pat-alice sessions = %d, want 1 (the expired one left out)", s.Sessions)
			}
		}
	}
	if !found {
		t.Errorf("signed in = %+v, want pat-alice", in)
	}
}
