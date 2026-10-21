package db

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Release problems: bugs tracked per release, which can be fixed in a later build and
// verified there. The QA checklist (test cases, results, sign-off, testing done) that
// used to feed them was removed on 21 Oct 2026; its tables stay but nothing reads them.

// ── Release problem reports ───────────────────────────────────────────────────

// ReleaseProblem is a tracked problem an operator filed against a release. Device is
// optional; the joined serial is populated for display.
type ReleaseProblem struct {
	ID            uuid.UUID
	ReleaseID     int
	DeviceID      *uuid.UUID
	DeviceSerial  string
	BuildID       string
	Title         string
	Description   string
	Severity      string // blocker | major | minor
	Status        string // open | fixed | verified | wontfix
	Source        string // manual (older rows may say qa: filed by the QA checklist, removed 21 Oct 2026)
	ReportedBy    string
	CreatedAt     time.Time
	UpdatedAt     time.Time

	// Continuity ("rides the release train"): a manual bug reported on one build can be
	// fixed in a later build and verified there. OriginVersion is release_id's version.
	OriginVersion       string // version the problem was first reported on (= ReleaseID)
	FixedInReleaseID    *int   // build a dev claims the fix landed in; nil until claimed
	FixedInVersion      string // joined version of FixedInReleaseID, "" when unset
	VerifiedInReleaseID *int   // build an operator confirmed the fix on; nil until verified
	VerifiedInVersion   string // joined version of VerifiedInReleaseID, "" when unset
	Inherited           bool   // true when surfaced on a release later than its origin (carried forward)
}

// ProblemSummary is the per-release rollup used by badges: how many problems are
// still open, and how many of those are blockers.
type ProblemSummary struct {
	Open     int
	Blockers int
	Total    int
}

var validProblemSeverity = map[string]bool{"blocker": true, "major": true, "minor": true}
var validProblemStatus = map[string]bool{"open": true, "fixed": true, "verified": true, "wontfix": true}

// A problem still counts as "open" (needs attention) unless it's verified-fixed
// or explicitly won't-fix.
func problemIsOpen(status string) bool { return status != "verified" && status != "wontfix" }

// CreateReleaseProblem files a new problem. deviceID may be nil.
func (d *DB) CreateReleaseProblem(ctx context.Context, p ReleaseProblem) (uuid.UUID, error) {
	if !validProblemSeverity[p.Severity] {
		p.Severity = "major"
	}
	var id uuid.UUID
	err := d.pool.QueryRow(ctx, `
		INSERT INTO release_problems
			(release_id, device_id, build_id, title, description, severity, reported_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		p.ReleaseID, p.DeviceID, p.BuildID, p.Title, p.Description, p.Severity, p.ReportedBy,
	).Scan(&id)
	return id, err
}

// releaseProblemCols / releaseProblemJoins are the canonical column list and joins for
// selecting problems with their origin/fixed/verified release versions and the joined
// device serial. Shared by ListReleaseProblems, CarriedForwardProblems and
// ListProblemBoard so the scan order stays in lock-step (see scanReleaseProblem).
const releaseProblemCols = `
	p.id, p.release_id, orel.version,
	p.device_id, COALESCE(d.serial_number, ''), p.build_id,
	p.title, p.description, p.severity, p.status, p.source, p.reported_by,
	p.fixed_in_release_id, COALESCE(fr.version, ''),
	p.verified_in_release_id, COALESCE(vr.version, ''),
	p.created_at, p.updated_at`

const releaseProblemJoins = `
	FROM release_problems p
	JOIN releases orel ON orel.id = p.release_id
	LEFT JOIN devices d ON d.id = p.device_id
	LEFT JOIN releases fr ON fr.id = p.fixed_in_release_id
	LEFT JOIN releases vr ON vr.id = p.verified_in_release_id`

// releaseProblemOrder sorts open-first, then blocker→minor, then newest.
const releaseProblemOrder = `
	ORDER BY
		(p.status NOT IN ('verified','wontfix')) DESC,
		CASE p.severity WHEN 'blocker' THEN 0 WHEN 'major' THEN 1 ELSE 2 END,
		p.created_at DESC`

// scanReleaseProblem scans one row selected with releaseProblemCols. It does NOT set
// Inherited — the caller sets that based on which release it queried for.
func scanReleaseProblem(rows pgx.Rows, p *ReleaseProblem) error {
	return rows.Scan(&p.ID, &p.ReleaseID, &p.OriginVersion,
		&p.DeviceID, &p.DeviceSerial, &p.BuildID,
		&p.Title, &p.Description, &p.Severity, &p.Status, &p.Source, &p.ReportedBy,
		&p.FixedInReleaseID, &p.FixedInVersion,
		&p.VerifiedInReleaseID, &p.VerifiedInVersion,
		&p.CreatedAt, &p.UpdatedAt)
}

// ListReleaseProblems returns a release's OWN (native) problems — those reported against
// it — open first then by severity (blocker → minor) and newest. Carried-over problems
// from earlier builds are returned separately by CarriedForwardProblems.
func (d *DB) ListReleaseProblems(ctx context.Context, releaseID int) ([]ReleaseProblem, error) {
	rows, err := d.pool.Query(ctx,
		`SELECT`+releaseProblemCols+releaseProblemJoins+`
		WHERE p.release_id = $1`+releaseProblemOrder, releaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReleaseProblem
	for rows.Next() {
		var p ReleaseProblem
		if err := scanReleaseProblem(rows, &p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CarriedForwardProblems returns manual problems that should follow the release train
// onto releaseID but were NOT reported against it: (a) problems that were reported fixed
// in an EARLIER build but whose verification then FAILED (reopened to 'open' while
// keeping fixed_in_release_id) — these ride forward until a fix holds — and (b) problems
// whose fix is claimed to land in THIS build (awaiting an operator's verification here). A
// plain still-open bug that was never claimed fixed does NOT carry; it stays on its origin
// build. All rows are flagged Inherited. Chronology is releases.created_at.
func (d *DB) CarriedForwardProblems(ctx context.Context, releaseID int) ([]ReleaseProblem, error) {
	rows, err := d.pool.Query(ctx,
		`SELECT`+releaseProblemCols+releaseProblemJoins+`
		WHERE p.release_id <> $1 AND p.source = 'manual'
		  AND NOT orel.is_branch                                        -- branch bugs never carry onto mainline
		  AND EXISTS (SELECT 1 FROM releases WHERE id = $1 AND NOT is_branch) -- branches inherit nothing
		  AND (
			p.fixed_in_release_id = $1                                  -- dev claimed fix in THIS build (verify here)
			OR (p.status = 'open' AND p.fixed_in_release_id IS NOT NULL
			    AND (SELECT created_at FROM releases WHERE id = p.fixed_in_release_id)
			        < (SELECT created_at FROM releases WHERE id = $1))
			    -- reported fixed in an earlier build but verification failed (reopened,
			    -- fixed_in kept); only rides onto builds created AFTER the claimed-fix
			    -- build — a build predating the fix claim can't have the fix pending
		)`+releaseProblemOrder, releaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReleaseProblem
	for rows.Next() {
		var p ReleaseProblem
		if err := scanReleaseProblem(rows, &p); err != nil {
			return nil, err
		}
		p.Inherited = true
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListProblemBoard returns every problem across all releases (open first), for the hub's
// cross-release "all the problems at a glance" board. Each row is canonical (origin =
// ReleaseID / OriginVersion); Inherited is left false.
func (d *DB) ListProblemBoard(ctx context.Context) ([]ReleaseProblem, error) {
	rows, err := d.pool.Query(ctx,
		`SELECT`+releaseProblemCols+releaseProblemJoins+`
		WHERE NOT orel.is_branch`+releaseProblemOrder) // branch bugs are off-mainline, not on the board
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReleaseProblem
	for rows.Next() {
		var p ReleaseProblem
		if err := scanReleaseProblem(rows, &p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdateReleaseProblem changes a problem's status and/or severity. Empty values
// leave the corresponding field unchanged.
func (d *DB) UpdateReleaseProblem(ctx context.Context, id uuid.UUID, status, severity string) error {
	if status != "" && !validProblemStatus[status] {
		return fmt.Errorf("invalid status %q", status)
	}
	if severity != "" && !validProblemSeverity[severity] {
		return fmt.Errorf("invalid severity %q", severity)
	}
	_, err := d.pool.Exec(ctx, `
		UPDATE release_problems
		SET status   = COALESCE(NULLIF($2, ''), status),
		    severity = COALESCE(NULLIF($3, ''), severity),
		    updated_at = NOW()
		WHERE id = $1`, id, status, severity)
	return err
}

// DeleteReleaseProblem removes a problem outright (admin only).
func (d *DB) DeleteReleaseProblem(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM release_problems WHERE id = $1`, id)
	return err
}

// MarkProblemFixedIn records that a dev landed the fix for a problem in fixedInReleaseID
// and moves it to 'fixed' (awaiting an operator's verification on that build). Passing 0
// clears the fixed-in link and reopens the problem.
func (d *DB) MarkProblemFixedIn(ctx context.Context, id uuid.UUID, fixedInReleaseID int) error {
	if fixedInReleaseID <= 0 {
		// Reopen: clear the whole fix/verify trail so the bug rides the train again.
		_, err := d.pool.Exec(ctx, `
			UPDATE release_problems
			SET fixed_in_release_id = NULL, verified_in_release_id = NULL,
			    status = 'open', updated_at = NOW()
			WHERE id = $1`, id)
		return err
	}
	_, err := d.pool.Exec(ctx, `
		UPDATE release_problems
		SET fixed_in_release_id = $2, status = 'fixed', updated_at = NOW()
		WHERE id = $1`, id, fixedInReleaseID)
	return err
}

// VerifyProblem marks a problem verified-fixed on verifiedInReleaseID (an operator confirmed
// the fix on that build). If no fixed-in build was recorded, the verifying build is taken
// as the fix build too.
func (d *DB) VerifyProblem(ctx context.Context, id uuid.UUID, verifiedInReleaseID int) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE release_problems
		SET verified_in_release_id = $2,
		    fixed_in_release_id    = COALESCE(fixed_in_release_id, $2),
		    status = 'verified', updated_at = NOW()
		WHERE id = $1`, id, verifiedInReleaseID)
	return err
}

// GlobalProblemSummary returns the open/blocker/total rollup across every release, for
// the hub's headline "all the problems at a glance" stat.
func (d *DB) GlobalProblemSummary(ctx context.Context) (ProblemSummary, error) {
	var s ProblemSummary
	err := d.pool.QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE status NOT IN ('verified','wontfix')),
			COUNT(*) FILTER (WHERE status NOT IN ('verified','wontfix') AND severity='blocker'),
			COUNT(*)
		FROM release_problems
		WHERE release_id IN (SELECT id FROM releases WHERE NOT is_branch)`).Scan(&s.Open, &s.Blockers, &s.Total)
	return s, err
}

// ReleaseProblemSummary returns the open/blocker/total counts for one release.
func (d *DB) ReleaseProblemSummary(ctx context.Context, releaseID int) (ProblemSummary, error) {
	var s ProblemSummary
	err := d.pool.QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE status NOT IN ('verified','wontfix')),
			COUNT(*) FILTER (WHERE status NOT IN ('verified','wontfix') AND severity='blocker'),
			COUNT(*)
		FROM release_problems WHERE release_id = $1`, releaseID).Scan(&s.Open, &s.Blockers, &s.Total)
	return s, err
}

// ProblemSummariesByRelease returns the per-release problem rollup for every
// release in one query, for the releases list badges (avoids an N+1).
func (d *DB) ProblemSummariesByRelease(ctx context.Context) (map[int]ProblemSummary, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT release_id,
			COUNT(*) FILTER (WHERE status NOT IN ('verified','wontfix')),
			COUNT(*) FILTER (WHERE status NOT IN ('verified','wontfix') AND severity='blocker'),
			COUNT(*)
		FROM release_problems GROUP BY release_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int]ProblemSummary)
	for rows.Next() {
		var rid int
		var s ProblemSummary
		if err := rows.Scan(&rid, &s.Open, &s.Blockers, &s.Total); err != nil {
			return nil, err
		}
		out[rid] = s
	}
	return out, rows.Err()
}

// CarriedProblemCountsByRelease returns, per non-branch release, the count of
// problems that CARRY onto it (mirrors the CarriedForwardProblems rule) — all
// carried problems are active/awaiting-verification, so they count as open. Used
// to make the release-list problem badge match the workspace board (native +
// carried) instead of counting only native problems.
func (d *DB) CarriedProblemCountsByRelease(ctx context.Context) (map[int]ProblemSummary, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT r.id, COUNT(*), COUNT(*) FILTER (WHERE p.severity = 'blocker')
		FROM releases r
		JOIN release_problems p ON p.source = 'manual' AND p.release_id <> r.id
		JOIN releases orel ON orel.id = p.release_id AND NOT orel.is_branch
		WHERE NOT r.is_branch
		  AND (
		     p.fixed_in_release_id = r.id
		     OR (p.status = 'open' AND p.fixed_in_release_id IS NOT NULL
		         AND (SELECT created_at FROM releases WHERE id = p.fixed_in_release_id) < r.created_at)
		  )
		GROUP BY r.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int]ProblemSummary)
	for rows.Next() {
		var rid int
		var s ProblemSummary
		if err := rows.Scan(&rid, &s.Open, &s.Blockers); err != nil {
			return nil, err
		}
		out[rid] = s
	}
	return out, rows.Err()
}
