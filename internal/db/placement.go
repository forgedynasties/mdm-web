package db


// The "to place" queue (user, 3 Nov 2026: "for ops they should see, by default, yet to assign
// devices and deployed … a queue for enrolled but not deployed"; picked Q1 · R2 · K2).
//
// No Lab state (user, 19 Nov 2026: dev is a staging copy of prod, lab devices are mocked as
// live venue devices): a device at no venue is To place, a device at a venue is In service,
// and nothing else. devices.placement is kept as a column (new devices get 'queue') but the
// state ignores it; a legacy device that was never queued reads as To place too.
//
//   R2  AIO Enroll's token may carry a placement; 'lab' (AIO Enroll for Android 0.5.0 still
//       sends it) is accepted and means 'queue'.
//
// Access: Ops lead manages To place (role grant); Field manages the devices they enrolled until
// they are placed (scope "own", K2), and may place those at any venue.
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
-- No setup visits (3 Nov 2026): the visit grant of Field is dropped. (It used to be replaced by
-- a Manage grant on the Setting up state; that state is gone, see below.)
WITH s AS (INSERT INTO access_seed (k) VALUES ('setup-bucket-v1') ON CONFLICT DO NOTHING RETURNING k)
DELETE FROM access_levels a USING s WHERE a.subject_type = 'role' AND a.subject = 'field' AND a.scope_type = 'visit';
-- Setting up and Hand over are gone (19 Nov 2026, user: "we dont need this complexity"): a device
-- at a venue is in service. Once: every grant on the Setting up state goes (Field's Manage on it
-- and the Ops lead's, which sits beside their grant on In service; any other grant on it would
-- have meant "devices at a venue" and must not quietly widen), and every device at a venue that
-- was still waiting for a hand-over is stamped, so nobody is left in a half state. Columns stay.
WITH s AS (INSERT INTO access_seed (k) VALUES ('no-setting-up-v1') ON CONFLICT DO NOTHING RETURNING k),
     del AS (DELETE FROM access_levels a USING s WHERE a.scope_type = 'state' AND a.state = 'setting_up' RETURNING a.id)
UPDATE devices d SET handed_over_at = now(), handed_over_by = 'migrated'
FROM s WHERE d.restaurant_id IS NOT NULL AND d.handed_over_at IS NULL;
-- No Lab state (19 Nov 2026), once (marker no-lab-v1):
--  * a device at no venue that was not queued (what Lab was) is retired the way the dashboard's
--    Retire does it (enrollment_status 'retired' + a device.retire audit entry, actor
--    'migration'): it leaves every list and count and can be un-retired from its page;
--  * enrollment profiles that placed into Lab place into the queue;
--  * the Engineer role is retired: its people become Support (operator), open sessions too;
--    the role's grants and any grant or Where on the Lab state go.
WITH s AS (INSERT INTO access_seed (k) VALUES ('no-lab-v1') ON CONFLICT DO NOTHING RETURNING k),
     ret AS (UPDATE devices d SET enrollment_status = 'retired' FROM s
             WHERE d.restaurant_id IS NULL AND NOT d.hidden AND d.placement <> 'queue'
               AND d.enrollment_status NOT IN ('retired', 'wiped', 'unenrolled')
             RETURNING d.id, d.serial_number),
     aud AS (INSERT INTO audit_log (actor, action, target, detail, device_id)
             SELECT 'migration', 'device.retire', serial_number, 'retired', id FROM ret RETURNING 1),
     prof AS (UPDATE enrollment_profiles p SET placement = 'queue' FROM s WHERE p.placement = 'lab' RETURNING 1),
     usr AS (UPDATE users u SET role = 'operator' FROM s WHERE u.role = 'engineer' RETURNING 1),
     ses AS (UPDATE sessions se SET role = 'operator' FROM s WHERE se.role = 'engineer' RETURNING 1),
     gr AS (DELETE FROM access_levels a USING s
            WHERE (a.subject_type = 'role' AND a.subject = 'engineer') OR a.state = 'lab' RETURNING 1),
     wh AS (DELETE FROM access_where w USING s WHERE w.scope = 'lab' RETURNING 1)
SELECT 1;
`

// PlacementQueue is the placement of a device waiting to be placed (devices.placement).
const PlacementQueue = "queue"

// NormalizePlacement is the placement to store for what a caller sent: 'lab' (old AIO Enroll
// builds) means the queue, '' stays '' (no answer), anything else is invalid.
func NormalizePlacement(p string) (string, bool) {
	switch p {
	case "":
		return "", true
	case PlacementQueue, "lab":
		return PlacementQueue, true
	}
	return "", false
}
