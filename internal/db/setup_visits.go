package db

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Device states and setup visits (roles replan, decided 30 Oct 2026).
//
// A device is in one of three states, and the state decides which roles see it:
//
//   - Lab         no venue (restaurant_id IS NULL): office, test benches, stock
//   - Setting up  at a venue, not handed over yet (handed_over_at IS NULL)
//   - In service  handed over (handed_over_at set)
//
// Setup belongs to the device, not the restaurant: a venue open for a year can get two
// new KDS, and only those two are setting up. Placing a device at a venue (or moving it to
// another) puts it back to setting up — a trigger clears handed_over_at whenever
// restaurant_id changes, so every path that places a device (Assign, AIO Enroll with a
// venue, backend placements) gets it without knowing.
//
// A setup visit gives Field people one venue until a date: while it is open they work on
// all of that venue's devices. It ends on its date, by hand, or once everything at the
// venue has been handed over.
const setupVisitsSchema = `
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'devices' AND column_name = 'handed_over_at') THEN
    ALTER TABLE devices ADD COLUMN handed_over_at TIMESTAMPTZ;
    ALTER TABLE devices ADD COLUMN handed_over_by TEXT NOT NULL DEFAULT '';
    -- Every device placed before device states existed is running at its venue.
    UPDATE devices SET handed_over_at = now(), handed_over_by = 'before setup states' WHERE restaurant_id IS NOT NULL;
  END IF;
END $$;
CREATE OR REPLACE FUNCTION devices_placement_reset() RETURNS trigger AS $$
BEGIN
  IF NEW.restaurant_id IS DISTINCT FROM OLD.restaurant_id THEN
    NEW.handed_over_at := NULL;
    NEW.handed_over_by := '';
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'devices_placement_reset') THEN
    CREATE TRIGGER devices_placement_reset BEFORE UPDATE OF restaurant_id ON devices
      FOR EACH ROW EXECUTE FUNCTION devices_placement_reset();
  END IF;
END $$;
CREATE TABLE IF NOT EXISTS setup_visits (
	id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	restaurant_id UUID NOT NULL REFERENCES restaurants(id) ON DELETE CASCADE,
	purpose       TEXT NOT NULL DEFAULT '',
	ends_at       TIMESTAMPTZ NOT NULL,
	opened_by     TEXT NOT NULL DEFAULT '',
	opened_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
	closed_at     TIMESTAMPTZ,
	closed_by     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_setup_visits_restaurant ON setup_visits (restaurant_id, opened_at DESC);
CREATE TABLE IF NOT EXISTS setup_visit_people (
	visit_id UUID NOT NULL REFERENCES setup_visits(id) ON DELETE CASCADE,
	username TEXT NOT NULL,
	PRIMARY KEY (visit_id, username)
);
CREATE INDEX IF NOT EXISTS idx_setup_visit_people_user ON setup_visit_people (username);
`

// DefaultVisitDays is a new setup visit's length (user, 30 Oct 2026: "3 days default editable").
const DefaultVisitDays = 3

// Device states.
const (
	StateLab       = "lab"
	StateSettingUp = "setting_up"
	StateInService = "in_service"
)

// DeviceState is the device's state from its venue and hand-over.
func DeviceState(restaurantID *uuid.UUID, handedOver bool) string {
	switch {
	case restaurantID == nil:
		return StateLab
	case handedOver:
		return StateInService
	}
	return StateSettingUp
}

// SetupVisit is one visit with its people.
type SetupVisit struct {
	ID           uuid.UUID
	RestaurantID uuid.UUID
	Restaurant   string
	Purpose      string
	EndsAt       time.Time
	OpenedBy     string
	OpenedAt     time.Time
	ClosedAt     *time.Time
	ClosedBy     string
	People       []string
}

// Open reports whether the visit is running now.
func (v SetupVisit) Open() bool { return v.ClosedAt == nil && time.Now().Before(v.EndsAt) }

const visitOpenSQL = `v.closed_at IS NULL AND v.ends_at > now()`

// OpenSetupVisit starts a visit at a venue for people until endsAt.
func (d *DB) OpenSetupVisit(ctx context.Context, restaurantID uuid.UUID, purpose string, endsAt time.Time, people []string, by string) (uuid.UUID, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `INSERT INTO setup_visits (restaurant_id, purpose, ends_at, opened_by) VALUES ($1, $2, $3, $4) RETURNING id`,
		restaurantID, purpose, endsAt, by).Scan(&id); err != nil {
		return uuid.Nil, err
	}
	for _, p := range people {
		if _, err := tx.Exec(ctx, `INSERT INTO setup_visit_people (visit_id, username) VALUES ($1, $2) ON CONFLICT DO NOTHING`, id, p); err != nil {
			return uuid.Nil, err
		}
	}
	return id, tx.Commit(ctx)
}

// ExtendSetupVisit moves an open visit's end date.
func (d *DB) ExtendSetupVisit(ctx context.Context, id uuid.UUID, endsAt time.Time) error {
	tag, err := d.pool.Exec(ctx, `UPDATE setup_visits SET ends_at = $2 WHERE id = $1 AND closed_at IS NULL`, id, endsAt)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrVisitNotOpen
	}
	return err
}

// CloseSetupVisit ends a visit now.
func (d *DB) CloseSetupVisit(ctx context.Context, id uuid.UUID, by string) error {
	tag, err := d.pool.Exec(ctx, `UPDATE setup_visits SET closed_at = now(), closed_by = $2 WHERE id = $1 AND closed_at IS NULL`, id, by)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrVisitNotOpen
	}
	return err
}

// ErrVisitNotOpen: the visit is unknown or already closed.
var ErrVisitNotOpen = errors.New("that visit is not open")

// SetVisitPeople replaces a visit's people.
func (d *DB) SetVisitPeople(ctx context.Context, id uuid.UUID, people []string) error {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM setup_visit_people WHERE visit_id = $1`, id); err != nil {
		return err
	}
	for _, p := range people {
		if _, err := tx.Exec(ctx, `INSERT INTO setup_visit_people (visit_id, username) VALUES ($1, $2) ON CONFLICT DO NOTHING`, id, p); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// listVisits loads visits matching where (with $1… args), newest first.
func (d *DB) listVisits(ctx context.Context, where string, args ...any) ([]SetupVisit, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT v.id, v.restaurant_id, COALESCE(r.name, ''), v.purpose, v.ends_at, v.opened_by, v.opened_at, v.closed_at, v.closed_by,
		       COALESCE((SELECT array_agg(p.username ORDER BY p.username) FROM setup_visit_people p WHERE p.visit_id = v.id), '{}')
		FROM setup_visits v LEFT JOIN restaurants r ON r.id = v.restaurant_id
		WHERE `+where+` ORDER BY v.opened_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SetupVisit
	for rows.Next() {
		var v SetupVisit
		if err := rows.Scan(&v.ID, &v.RestaurantID, &v.Restaurant, &v.Purpose, &v.EndsAt, &v.OpenedBy, &v.OpenedAt, &v.ClosedAt, &v.ClosedBy, &v.People); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// VenueVisits lists a venue's visits, open ones and the last ten closed.
func (d *DB) VenueVisits(ctx context.Context, restaurantID uuid.UUID) ([]SetupVisit, error) {
	return d.listVisits(ctx, `v.restaurant_id = $1 AND (`+visitOpenSQL+` OR v.id IN (
		SELECT id FROM setup_visits WHERE restaurant_id = $1 ORDER BY opened_at DESC LIMIT 10))`, restaurantID)
}

// OpenVisitsFor lists the open visits a person is on.
func (d *DB) OpenVisitsFor(ctx context.Context, username string) ([]SetupVisit, error) {
	return d.listVisits(ctx, visitOpenSQL+` AND EXISTS (SELECT 1 FROM setup_visit_people p WHERE p.visit_id = v.id AND p.username = $1)`, username)
}

// OpenVisits lists every open visit.
func (d *DB) OpenVisits(ctx context.Context) ([]SetupVisit, error) {
	return d.listVisits(ctx, visitOpenSQL)
}

// GetSetupVisit loads one visit.
func (d *DB) GetSetupVisit(ctx context.Context, id uuid.UUID) (*SetupVisit, error) {
	vs, err := d.listVisits(ctx, `v.id = $1`, id)
	if err != nil {
		return nil, err
	}
	if len(vs) == 0 {
		return nil, pgx.ErrNoRows
	}
	return &vs[0], nil
}

// VisitVenuesFor is the venues where the person is on an open visit (the Field scope).
func (d *DB) VisitVenuesFor(ctx context.Context, username string) (map[uuid.UUID]bool, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT DISTINCT v.restaurant_id FROM setup_visits v JOIN setup_visit_people p ON p.visit_id = v.id
		WHERE `+visitOpenSQL+` AND p.username = $1`, username)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// HandOver moves setting-up devices to in service. Only devices at a venue and not yet
// handed over change; it returns how many did. A venue left with nothing setting up
// closes its open visits that saw a hand-over (the visit's work is done).
func (d *DB) HandOver(ctx context.Context, ids []uuid.UUID, by string) (int64, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE devices SET handed_over_at = now(), handed_over_by = $2
		WHERE id = ANY($1) AND restaurant_id IS NOT NULL AND handed_over_at IS NULL`, ids, by)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE setup_visits v SET closed_at = now(), closed_by = 'all handed over'
		WHERE `+visitOpenSQL+`
		  AND v.restaurant_id IN (SELECT restaurant_id FROM devices WHERE id = ANY($1) AND restaurant_id IS NOT NULL)
		  AND NOT EXISTS (SELECT 1 FROM devices d WHERE d.restaurant_id = v.restaurant_id AND d.handed_over_at IS NULL AND NOT d.hidden)
		  AND EXISTS (SELECT 1 FROM devices d WHERE d.restaurant_id = v.restaurant_id AND d.handed_over_at >= v.opened_at)`, ids); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), tx.Commit(ctx)
}

// DeviceHandOver is when and by whom the device was handed over (nil when it wasn't).
func (d *DB) DeviceHandOver(ctx context.Context, id uuid.UUID) (*time.Time, string, error) {
	var at *time.Time
	var by string
	err := d.pool.QueryRow(ctx, `SELECT handed_over_at, handed_over_by FROM devices WHERE id = $1`, id).Scan(&at, &by)
	return at, by, err
}

// HandedOverSince counts devices handed over at or after t.
func (d *DB) HandedOverSince(ctx context.Context, t time.Time) (int, error) {
	var n int
	err := d.pool.QueryRow(ctx, `SELECT COUNT(*) FROM devices WHERE handed_over_at >= $1 AND handed_over_by <> 'before setup states'`, t).Scan(&n)
	return n, err
}
