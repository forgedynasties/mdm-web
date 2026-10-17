package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ── Test team / QA ──────────────────────────────────────────────────────────────

// TestCase is a single thing to verify. Base cases apply to every release; cases with a
// ReleaseID apply only to that release.
type TestCase struct {
	ID        uuid.UUID `json:"id"`
	Title     string    `json:"title"`
	Area      string    `json:"area"`
	Steps     string    `json:"steps"`
	Expected  string    `json:"expected_result"`
	Base      bool      `json:"base"`
	ReleaseID *int      `json:"release_id"`
	Active    bool      `json:"active"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

// ChecklistItem is one test case joined with its result for a given release.
type ChecklistItem struct {
	TestCase
	Status   string     `json:"status"` // untested|pass|fail|blocked|skip
	Notes    string     `json:"notes"`
	TestedBy string     `json:"tested_by"`
	TestedAt *time.Time `json:"tested_at"`
}

// QASummary holds the per-release status counts.
type QASummary struct {
	Total, Pass, Fail, Blocked, Skip, Untested int
}

// Passed reports whether every applicable case passed (and there is at least one).
func (q QASummary) Passed() bool { return q.Total > 0 && q.Pass == q.Total }

// CreateTestCase inserts a base (releaseID nil) or release-specific test case.
func (d *DB) CreateTestCase(ctx context.Context, tc TestCase) (uuid.UUID, error) {
	var id uuid.UUID
	err := d.pool.QueryRow(ctx, `
		INSERT INTO test_cases (title, area, steps, expected_result, base, release_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		tc.Title, tc.Area, tc.Steps, tc.Expected, tc.Base, tc.ReleaseID, tc.CreatedBy).Scan(&id)
	return id, err
}

// EnsureFixVerificationCase adds a "Verify fix: …" case to releaseID's checklist for the
// given problem (so the test team confirms the fix), unless one already exists for that
// (release, problem). Title/steps are taken from the problem. Called when a carried-over
// bug is marked "fixed here".
func (d *DB) EnsureFixVerificationCase(ctx context.Context, releaseID int, problemID uuid.UUID, createdBy string) error {
	// Advisory lock so two concurrent submits can't both pass NOT EXISTS and insert
	// duplicate "Verify fix" cases (there's no unique constraint to lean on).
	return d.withAdvisoryLock(ctx, advisoryKeyIntUUID(releaseID, problemID), func(ctx context.Context) error {
		_, err := d.pool.Exec(ctx, `
			INSERT INTO test_cases (title, area, steps, expected_result, base, release_id, created_by, problem_id)
			SELECT 'Verify fix: ' || p.title, 'Regression',
			       COALESCE(NULLIF(p.description, ''), 'Confirm the reported issue no longer occurs.'),
			       'Issue no longer reproduces.', false, $1, $2, p.id
			FROM release_problems p
			WHERE p.id = $3
			  AND NOT EXISTS (SELECT 1 FROM test_cases WHERE release_id = $1 AND problem_id = $3)`,
			releaseID, createdBy, problemID)
		return err
	})
}

// VerifyProblemForCase verifies the problem linked to a test case (if any) on releaseID —
// called when that "Verify fix" case is marked pass, closing the loop that a manual
// carried-over bug is fixed and confirmed. No-op when the case links no problem.
func (d *DB) VerifyProblemForCase(ctx context.Context, caseID uuid.UUID, releaseID int) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE release_problems
		SET status = 'verified', verified_in_release_id = $2,
		    fixed_in_release_id = COALESCE(fixed_in_release_id, $2), updated_at = NOW()
		WHERE id = (SELECT problem_id FROM test_cases WHERE id = $1 AND problem_id IS NOT NULL)`,
		caseID, releaseID)
	return err
}

// ReopenProblemForCase reopens the problem linked to a test case (if any) — called when a
// "Verify fix" case fails, meaning the fix didn't hold. The bug goes back to open but its
// fixed_in_release_id is KEPT (it records the dev's claim, which is now disputed), so the
// trail reads "reported v2.0.2 → dev says fixed in v2.0.3 → QA says still broken".
// The carry-forward query handles open bugs with a fixed_in set.
// Returns true when a linked problem was found and reopened.
func (d *DB) ReopenProblemForCase(ctx context.Context, caseID uuid.UUID) (bool, error) {
	tag, err := d.pool.Exec(ctx, `
		UPDATE release_problems
		SET status = 'open', updated_at = NOW()
		WHERE id = (SELECT problem_id FROM test_cases WHERE id = $1 AND problem_id IS NOT NULL)`,
		caseID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// UpdateTestCase edits an existing case's content and active flag.
func (d *DB) UpdateTestCase(ctx context.Context, id uuid.UUID, title, area, steps, expected string, active bool) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE test_cases SET title=$2, area=$3, steps=$4, expected_result=$5, active=$6 WHERE id=$1`,
		id, title, area, steps, expected, active)
	return err
}

// DeleteTestCase removes a case (and its results, via FK cascade).
func (d *DB) DeleteTestCase(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM test_cases WHERE id=$1`, id)
	return err
}

func scanTestCases(rows pgx.Rows) ([]TestCase, error) {
	defer rows.Close()
	var out []TestCase
	for rows.Next() {
		var t TestCase
		if err := rows.Scan(&t.ID, &t.Title, &t.Area, &t.Steps, &t.Expected, &t.Base, &t.ReleaseID, &t.Active, &t.CreatedBy, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListBaseTestCases returns the standard cases (optionally only active ones).
func (d *DB) ListBaseTestCases(ctx context.Context, onlyActive bool) ([]TestCase, error) {
	q := `SELECT id, title, area, steps, expected_result, base, release_id, active, created_by, created_at
	      FROM test_cases WHERE base = true`
	if onlyActive {
		q += ` AND active = true`
	}
	q += ` ORDER BY area, title`
	rows, err := d.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	return scanTestCases(rows)
}

// ListReleaseTestCases returns the cases specific to one release.
func (d *DB) ListReleaseTestCases(ctx context.Context, releaseID int) ([]TestCase, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, title, area, steps, expected_result, base, release_id, active, created_by, created_at
		FROM test_cases WHERE release_id = $1 ORDER BY area, title`, releaseID)
	if err != nil {
		return nil, err
	}
	return scanTestCases(rows)
}

// GetReleaseChecklist returns the applicable cases for a release (active base cases plus
// this release's own cases) joined with any recorded result.
func (d *DB) GetReleaseChecklist(ctx context.Context, releaseID int) ([]ChecklistItem, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT tc.id, tc.title, tc.area, tc.steps, tc.expected_result, tc.base, tc.release_id,
		       tc.active, tc.created_by, tc.created_at,
		       COALESCE(r.status, 'untested'), COALESCE(r.notes, ''), COALESCE(r.tested_by, ''), r.tested_at
		FROM test_cases tc
		LEFT JOIN release_test_results r ON r.test_case_id = tc.id AND r.release_id = $1
		WHERE tc.active AND (tc.release_id = $1
		      OR (tc.base AND NOT COALESCE((SELECT skip_base_tests FROM releases WHERE id = $1), false)))
		ORDER BY tc.base DESC, tc.area, tc.title`, releaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChecklistItem
	for rows.Next() {
		var c ChecklistItem
		if err := rows.Scan(&c.ID, &c.Title, &c.Area, &c.Steps, &c.Expected, &c.Base, &c.ReleaseID,
			&c.Active, &c.CreatedBy, &c.CreatedAt,
			&c.Status, &c.Notes, &c.TestedBy, &c.TestedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetTestResult records (upserts) an operator's outcome for one case on one release.
func (d *DB) SetTestResult(ctx context.Context, releaseID int, testCaseID uuid.UUID, status, notes, testedBy string) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO release_test_results (release_id, test_case_id, status, notes, tested_by, tested_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
		ON CONFLICT (release_id, test_case_id)
		DO UPDATE SET status = $3, notes = $4, tested_by = $5, tested_at = NOW()`,
		releaseID, testCaseID, status, notes, testedBy)
	return err
}

// ReleaseQASummary computes the status counts for a release's checklist.
func (d *DB) ReleaseQASummary(ctx context.Context, releaseID int) (QASummary, error) {
	var s QASummary
	err := d.pool.QueryRow(ctx, `
		SELECT
			COUNT(*),
			COUNT(*) FILTER (WHERE status='pass'),
			COUNT(*) FILTER (WHERE status='fail'),
			COUNT(*) FILTER (WHERE status='blocked'),
			COUNT(*) FILTER (WHERE status='skip'),
			COUNT(*) FILTER (WHERE status='untested')
		FROM (
			SELECT COALESCE(r.status, 'untested') AS status
			FROM test_cases tc
			LEFT JOIN release_test_results r ON r.test_case_id = tc.id AND r.release_id = $1
			WHERE tc.active AND (tc.release_id = $1
			      OR (tc.base AND NOT COALESCE((SELECT skip_base_tests FROM releases WHERE id = $1), false)))
		) c`, releaseID).Scan(&s.Total, &s.Pass, &s.Fail, &s.Blocked, &s.Skip, &s.Untested)
	return s, err
}

// ── Release problem reports ───────────────────────────────────────────────────

// ReleaseProblem is a tracked problem an operator filed against a release. TestCase
// and Device are optional; the joined title/serial are populated for display.
type ReleaseProblem struct {
	ID            uuid.UUID
	ReleaseID     int
	TestCaseID    *uuid.UUID
	TestCaseTitle string
	DeviceID      *uuid.UUID
	DeviceSerial  string
	BuildID       string
	Title         string
	Description   string
	Severity      string // blocker | major | minor
	Status        string // open | fixed | verified | wontfix
	Source        string // manual | qa (auto-created from a failed QA case)
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

// CreateReleaseProblem files a new problem. testCaseID/deviceID may be nil.
func (d *DB) CreateReleaseProblem(ctx context.Context, p ReleaseProblem) (uuid.UUID, error) {
	if !validProblemSeverity[p.Severity] {
		p.Severity = "major"
	}
	var id uuid.UUID
	err := d.pool.QueryRow(ctx, `
		INSERT INTO release_problems
			(release_id, test_case_id, device_id, build_id, title, description, severity, reported_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
		p.ReleaseID, p.TestCaseID, p.DeviceID, p.BuildID, p.Title, p.Description, p.Severity, p.ReportedBy,
	).Scan(&id)
	return id, err
}

// releaseProblemCols / releaseProblemJoins are the canonical column list and joins for
// selecting problems with their origin/fixed/verified release versions and the joined
// case title + device serial. Shared by ListReleaseProblems, CarriedForwardProblems and
// ListProblemBoard so the scan order stays in lock-step (see scanReleaseProblem).
const releaseProblemCols = `
	p.id, p.release_id, orel.version,
	p.test_case_id, COALESCE(tc.title, ''),
	p.device_id, COALESCE(d.serial_number, ''), p.build_id,
	p.title, p.description, p.severity, p.status, p.source, p.reported_by,
	p.fixed_in_release_id, COALESCE(fr.version, ''),
	p.verified_in_release_id, COALESCE(vr.version, ''),
	p.created_at, p.updated_at`

const releaseProblemJoins = `
	FROM release_problems p
	JOIN releases orel ON orel.id = p.release_id
	LEFT JOIN test_cases tc ON tc.id = p.test_case_id
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
		&p.TestCaseID, &p.TestCaseTitle,
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
// in an EARLIER build but whose QA verification then FAILED (reopened to 'open' while
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
			    -- reported fixed in an earlier build but QA verification failed (reopened,
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

// UpsertQAProblem is called when a QA test case is marked failed: it opens a
// linked problem for (release, test_case) with source='qa', or reopens the
// existing QA problem if it had been resolved (a re-fail after a fix). Title comes
// from the case, build from the release; severity defaults to major.
func (d *DB) UpsertQAProblem(ctx context.Context, releaseID int, testCaseID uuid.UUID, notes, reportedBy string) error {
	if strings.TrimSpace(notes) == "" {
		notes = "Marked failed in QA."
	}
	// Advisory lock so two operators failing the same case at once can't both pass
	// NOT EXISTS and insert duplicate QA problems (no unique constraint exists).
	if err := d.withAdvisoryLock(ctx, advisoryKeyIntUUID(releaseID, testCaseID), func(ctx context.Context) error {
		_, err := d.pool.Exec(ctx, `
			INSERT INTO release_problems
				(release_id, test_case_id, build_id, title, description, severity, status, reported_by, source)
			SELECT $1, $2,
			       COALESCE((SELECT version FROM releases WHERE id = $1), ''),
			       'QA fail: ' || COALESCE((SELECT title FROM test_cases WHERE id = $2), 'test case'),
			       $3, 'major', 'open', $4, 'qa'
			WHERE NOT EXISTS (
				SELECT 1 FROM release_problems WHERE release_id = $1 AND test_case_id = $2 AND source = 'qa')
		`, releaseID, testCaseID, notes, reportedBy)
		return err
	}); err != nil {
		return err
	}
	// Reopen (and refresh notes on) an existing QA problem that had been closed.
	_, err := d.pool.Exec(ctx, `
		UPDATE release_problems SET status = 'open', description = $3, updated_at = NOW()
		WHERE release_id = $1 AND test_case_id = $2 AND source = 'qa'
		  AND status IN ('verified', 'wontfix')
	`, releaseID, testCaseID, notes)
	return err
}

// ResolveQAProblem is called when a QA case is marked passed: it marks the linked
// QA-sourced problem verified (two-way sync). No-op if there is none or it's
// already closed.
func (d *DB) ResolveQAProblem(ctx context.Context, releaseID int, testCaseID uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE release_problems SET status = 'verified', updated_at = NOW()
		WHERE release_id = $1 AND test_case_id = $2 AND source = 'qa'
		  AND status NOT IN ('verified', 'wontfix')
	`, releaseID, testCaseID)
	return err
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

// ActiveRelease returns the release currently "under test" — the newest non-hidden
// release whose testing is NOT yet done (testing_done_at IS NULL), preferring a draft.
// Marking testing done retires a release from this slot so the next build takes over. It
// anchors the hub focus band and is the default carry-forward target. Returns (nil, nil)
// when every release is hidden or finished.
func (d *DB) ActiveRelease(ctx context.Context) (*Release, error) {
	var r Release
	err := d.pool.QueryRow(ctx, `
		SELECT id, version, name, changelog, status, hidden, skip_base_tests, created_at, published_at,
		       signed_off_by, signed_off_at, testing_done_at, testing_done_by
		FROM releases
		WHERE NOT hidden AND testing_done_at IS NULL AND NOT is_branch
		ORDER BY (status = 'draft') DESC, created_at DESC
		LIMIT 1`).Scan(&r.ID, &r.Version, &r.Name, &r.Changelog, &r.Status, &r.Hidden, &r.SkipBaseTests,
		&r.CreatedAt, &r.PublishedAt, &r.SignedOffBy, &r.SignedOffAt, &r.TestingDoneAt, &r.TestingDoneBy)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
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
