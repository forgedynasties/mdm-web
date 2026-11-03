package db

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// The "to place" queue (user, 3 Nov 2026: "for ops they should see, by default, yet to assign
// devices and deployed … if they enroll a device … they should see a queue for enrolled but not
// deployed, then they can deploy them to venues assign roles create venues"; picked Q1 · R2 · K2).
//
// A device at no venue is either Lab (an engineer's bench device) or To place (enrolled for a
// restaurant, not placed yet). devices.placement says which: 'queue' = to place, anything else
// = Lab. It matters only while restaurant_id is NULL; a device taken off its venue goes back to
// whatever it was.
//
//   R2  AIO Enroll asks "Where is this device going?": a venue, "Not decided yet" (queue) or
//       "Lab"; the answer rides on the app's enrollment token (enrollment_profiles.placement).
//   R1  A token that doesn't say (older AIO Enroll builds, dashboard profiles): enrolled by an
//       Ops lead, Support, Field or Viewer → queue; by an engineer, a super admin or nobody (the
//       firmware client enrolls itself) → Lab. Devices already waiting were sorted once by R1.
//
// Access: Ops lead manages To place (role grant); Field manages the devices they enrolled until
// those are handed over (scope "own", K2), and may place those at any venue.
const placementSchema = `
ALTER TABLE devices ADD COLUMN IF NOT EXISTS placement TEXT NOT NULL DEFAULT '';
ALTER TABLE enrollment_profiles ADD COLUMN IF NOT EXISTS placement TEXT NOT NULL DEFAULT '';
DO $$ BEGIN
	IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'access_levels_scope_type_check'
	               AND pg_get_constraintdef(oid) LIKE '%own%') THEN
		ALTER TABLE access_levels DROP CONSTRAINT IF EXISTS access_levels_scope_type_check;
		ALTER TABLE access_levels ADD CONSTRAINT access_levels_scope_type_check
			CHECK (scope_type IN ('all', 'state', 'group', 'venue', 'visit', 'own'));
	END IF;
END $$;
WITH s AS (INSERT INTO access_seed (k) VALUES ('to-place-v1') ON CONFLICT DO NOTHING RETURNING k),
     q AS (UPDATE devices d SET placement = 'queue' FROM s, users u
           WHERE d.restaurant_id IS NULL AND d.placement = '' AND u.username = d.enrolled_by
             AND u.role NOT IN ('engineer', 'admin', 'owner', 'staff') RETURNING d.id)
INSERT INTO access_levels (subject_type, subject, scope_type, state, level, created_by)
SELECT 'role', v.role, v.scope, v.state, 'manage', 'seed' FROM s CROSS JOIN (VALUES
	('fleet_manager', 'state', 'to_place'),
	('field',         'own',   '')
) AS v(role, scope, state)
ON CONFLICT DO NOTHING;
`

// Placement values (devices.placement, enrollment_profiles.placement).
const (
	PlacementQueue = "queue"
	PlacementLab   = "lab"
)

// r1Placement is R1 as SQL for the enrolling account in param (e.g. "$7"): queue unless an
// engineer, a super admin, a customer or nobody enrolled it.
func r1Placement(param string) string {
	return `CASE WHEN EXISTS (SELECT 1 FROM users u WHERE u.username = ` + param + ` AND ` + param + ` <> ''
	AND u.role NOT IN ('engineer', 'admin', 'owner', 'staff')) THEN 'queue' ELSE 'lab' END`
}

// SetDevicePlacement moves a device at no venue between the queue and Lab ("Send to Lab",
// "Move to queue").
func (d *DB) SetDevicePlacement(ctx context.Context, id uuid.UUID, placement string) error {
	if placement != PlacementQueue && placement != PlacementLab {
		return fmt.Errorf("placement %q", placement)
	}
	_, err := d.pool.Exec(ctx, `UPDATE devices SET placement = $2 WHERE id = $1`, id, placement)
	return err
}

// DevicePlacement is a device's placement ('' = never set, which is Lab).
func (d *DB) DevicePlacement(ctx context.Context, id uuid.UUID) (string, error) {
	var p string
	err := d.pool.QueryRow(ctx, `SELECT placement FROM devices WHERE id = $1`, id).Scan(&p)
	return p, err
}
