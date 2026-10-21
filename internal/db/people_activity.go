package db

import (
	"context"
	"fmt"
	"time"
)

// PeopleDay is one day of what people (users with an account) did on the dashboard:
// audit actions, page views not counted, and how many of them did anything at all.
type PeopleDay struct {
	Day     time.Time
	Actions int
	People  int
}

// PeopleActivityDaily returns the last `days` days, oldest first, every day present.
// It feeds the Users page (P1, 21 Oct 2026): the rail's bars and the cards' counts.
func (d *DB) PeopleActivityDaily(ctx context.Context, days int) ([]PeopleDay, error) {
	if days < 1 {
		days = 8
	}
	rows, err := d.pool.Query(ctx, `
		WITH a AS (
			SELECT (al.created_at AT TIME ZONE 'UTC')::date AS day, al.actor, al.action
			FROM audit_log al JOIN users u ON u.username = al.actor
			WHERE al.created_at >= CURRENT_DATE - ($1::int - 1)
		)
		SELECT g.day::date,
		       COALESCE((SELECT COUNT(*) FROM a WHERE a.day = g.day::date AND a.action <> $2), 0),
		       COALESCE((SELECT COUNT(DISTINCT actor) FROM a WHERE a.day = g.day::date), 0)
		FROM generate_series(CURRENT_DATE - ($1::int - 1), CURRENT_DATE, interval '1 day') AS g(day)
		ORDER BY 1`, days, PageViewAction)
	if err != nil {
		return nil, fmt.Errorf("people activity: %w", err)
	}
	defer rows.Close()
	var out []PeopleDay
	for rows.Next() {
		var p PeopleDay
		if err := rows.Scan(&p.Day, &p.Actions, &p.People); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ActorWeek is one person's last 7 days next to the 7 before: actions (page views not
// counted) and page views. The individual user page's Actions card (I1).
type ActorWeek struct {
	Actions, PageViews, PrevActions int
}

func (d *DB) ActorWeekCounts(ctx context.Context, username string) (ActorWeek, error) {
	var w ActorWeek
	err := d.pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE created_at >= NOW() - interval '7 days' AND action <> $2),
		       COUNT(*) FILTER (WHERE created_at >= NOW() - interval '7 days' AND action = $2),
		       COUNT(*) FILTER (WHERE created_at < NOW() - interval '7 days' AND action <> $2)
		FROM audit_log WHERE actor = $1 AND created_at >= NOW() - interval '14 days'`,
		username, PageViewAction).Scan(&w.Actions, &w.PageViews, &w.PrevActions)
	if err != nil {
		return w, fmt.Errorf("actor week: %w", err)
	}
	return w, nil
}

// SignedIn is a person with a session that has not expired, and when they were last
// seen on it (the newest of their sessions).
type SignedIn struct {
	Username string
	LastSeen time.Time
	Sessions int
}

// SignedInNow lists everyone with a live session, most recently seen first. Settings ›
// People & sign-in (S1, 21 Oct 2026) shows it beside "Sign everyone out".
func (d *DB) SignedInNow(ctx context.Context) ([]SignedIn, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT username, MAX(last_seen), COUNT(*)
		FROM sessions WHERE expires_at > NOW() AND username <> ''
		GROUP BY username ORDER BY MAX(last_seen) DESC`)
	if err != nil {
		return nil, fmt.Errorf("signed in now: %w", err)
	}
	defer rows.Close()
	var out []SignedIn
	for rows.Next() {
		var s SignedIn
		if err := rows.Scan(&s.Username, &s.LastSeen, &s.Sessions); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
