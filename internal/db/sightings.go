package db

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Sightings: devices a restaurant's scout found on its Wi-Fi. See the migration in db.go
// and internal/scout for the flow.

// Sighting is one device seen on a venue's Wi-Fi, as the inbox renders it.
type Sighting struct {
	ID              uuid.UUID
	RestaurantID    uuid.UUID
	RestaurantName  string // joined
	ScoutSerial     string
	Host            string
	Port            int
	Serial          string
	Manufacturer    string
	Model           string
	Android         string
	OwnerPkg        string
	OwnerOurs       bool
	Accounts        int
	Users           int
	DPCVersion      string
	FirmwareVersion string
	ClassGuess      string
	KeyLabel        string // which of our adb keys it accepted; "" when the scout is older
	State           string
	Reason          string
	Step            int
	ApprovedBy      string
	ApprovedAt      *time.Time
	JobID           string
	FirstSeen       time.Time
	LastSeen        time.Time
}

// Name is the display name, dropping a maker the model already repeats.
func (s Sighting) Name() string {
	// A host that refused the key tells us nothing about itself, so it is named by its
	// address — that is all anybody has to go on until it is authorized.
	if s.Manufacturer == "" && s.Model == "" && s.Serial == "" {
		return s.Host
	}
	if s.Model == "" {
		return s.Manufacturer
	}
	if s.Manufacturer == "" {
		return s.Model
	}
	return s.Manufacturer + " " + s.Model
}

// SightingUpsert is what a net_sighting carries.
type SightingUpsert struct {
	RestaurantID    uuid.UUID
	ScoutSerial     string
	Host            string
	Port            int
	Serial          string
	Manufacturer    string
	Model           string
	Android         string
	OwnerPkg        string
	OwnerOurs       bool
	Accounts        int
	Users           int
	DPCVersion      string
	FirmwareVersion string
	ClassGuess      string
	KeyLabel        string // which of our adb keys it accepted
	State           string // ready | blocked | ours
	Reason          string
}

// UpsertSighting records one device seen on a scan. It never overwrites a row that is
// mid-enrolment or already enrolled — a late scan frame must not undo a job in flight.
func (d *DB) UpsertSighting(ctx context.Context, s SightingUpsert) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO sightings (restaurant_id, scout_serial, host, port, serial, manufacturer, model,
		    android, owner_pkg, owner_ours, accounts, users, dpc_version, firmware_version,
		    class_guess, key_label, state, reason, last_seen)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,NOW())
		ON CONFLICT (restaurant_id, serial) WHERE serial <> '' DO UPDATE SET
		    scout_serial = EXCLUDED.scout_serial, host = EXCLUDED.host, port = EXCLUDED.port,
		    manufacturer = EXCLUDED.manufacturer, model = EXCLUDED.model, android = EXCLUDED.android,
		    owner_pkg = EXCLUDED.owner_pkg, owner_ours = EXCLUDED.owner_ours,
		    accounts = EXCLUDED.accounts, users = EXCLUDED.users,
		    dpc_version = EXCLUDED.dpc_version, firmware_version = EXCLUDED.firmware_version,
		    class_guess = EXCLUDED.class_guess, last_seen = NOW(),
		    -- Keep the last label that worked when a newer scout cannot say.
		    key_label = CASE WHEN EXCLUDED.key_label <> '' THEN EXCLUDED.key_label ELSE sightings.key_label END,
		    state = CASE WHEN sightings.state IN ('enrolling','enrolled') THEN sightings.state ELSE EXCLUDED.state END,
		    reason = CASE WHEN sightings.state IN ('enrolling','enrolled') THEN sightings.reason ELSE EXCLUDED.reason END`,
		s.RestaurantID, s.ScoutSerial, s.Host, s.Port, s.Serial, s.Manufacturer, s.Model,
		s.Android, s.OwnerPkg, s.OwnerOurs, s.Accounts, s.Users, s.DPCVersion, s.FirmwareVersion,
		s.ClassGuess, s.KeyLabel, s.State, s.Reason)
	return err
}

// UpsertUnauthorized records a host that speaks adb but refused the fleet key. There is
// no serial to key on — props cannot be read without auth — so the address is the key,
// and an existing row for that address is only refreshed. A row that already carries a
// serial is left alone: the device was authorized at some point, which is the better
// information, and a later refusal (someone revoked the key on it) should not erase it.
func (d *DB) UpsertUnauthorized(ctx context.Context, restaurantID uuid.UUID, scoutSerial, host string, port int, reason string) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO sightings (restaurant_id, scout_serial, host, port, serial, state, reason, last_seen)
		VALUES ($1, $2, $3, $4, '', 'unauthorized', $5, NOW())
		ON CONFLICT (restaurant_id, host) WHERE serial = '' DO UPDATE SET
		    scout_serial = EXCLUDED.scout_serial, port = EXCLUDED.port, last_seen = NOW(),
		    state = CASE WHEN sightings.state IN ('enrolling','enrolled') THEN sightings.state ELSE 'unauthorized' END,
		    reason = CASE WHEN sightings.state IN ('enrolling','enrolled') THEN sightings.reason ELSE EXCLUDED.reason END`,
		restaurantID, scoutSerial, host, port, reason)
	return err
}

const sightingCols = `s.id, s.restaurant_id, COALESCE(r.name,''), s.scout_serial, s.host, s.port, s.serial,
	s.manufacturer, s.model, s.android, s.owner_pkg, s.owner_ours, s.accounts, s.users,
	s.dpc_version, s.firmware_version, s.class_guess, s.key_label, s.state, s.reason, s.step,
	s.approved_by, s.approved_at, s.job_id, s.first_seen, s.last_seen`

func scanSighting(row interface {
	Scan(dest ...any) error
}) (Sighting, error) {
	var s Sighting
	err := row.Scan(&s.ID, &s.RestaurantID, &s.RestaurantName, &s.ScoutSerial, &s.Host, &s.Port, &s.Serial,
		&s.Manufacturer, &s.Model, &s.Android, &s.OwnerPkg, &s.OwnerOurs, &s.Accounts, &s.Users,
		&s.DPCVersion, &s.FirmwareVersion, &s.ClassGuess, &s.KeyLabel, &s.State, &s.Reason, &s.Step,
		&s.ApprovedBy, &s.ApprovedAt, &s.JobID, &s.FirstSeen, &s.LastSeen)
	return s, err
}

// ListSightings returns every sighting not yet enrolled, newest venue activity first then
// ready devices first within a venue — the order the inbox card renders them. "ours"
// (firmware devices that enrol themselves) are excluded: nothing to do with them here.
//
// "unauthorized" is excluded too. Those rows are every host on the venue's network that
// speaks adb without trusting our key — a laptop, somebody's phone, a device from
// another fleet — and nobody can act on them from here: no serial, no model, nothing to
// install until somebody physically accepts the key. They stay in the table because
// they answer "why did the scout not pick that one up", but they are not inbox work.
func (d *DB) ListSightings(ctx context.Context) ([]Sighting, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT `+sightingCols+`
		FROM sightings s JOIN restaurants r ON r.id = s.restaurant_id
		WHERE s.state NOT IN ('enrolled', 'ours', 'unauthorized')
		ORDER BY r.name,
		  CASE s.state WHEN 'enrolling' THEN 0 WHEN 'ready' THEN 1 WHEN 'failed' THEN 2 ELSE 3 END,
		  s.model, s.serial`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sighting
	for rows.Next() {
		s, err := scanSighting(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetSighting fetches one by id.
func (d *DB) GetSighting(ctx context.Context, id uuid.UUID) (*Sighting, error) {
	row := d.pool.QueryRow(ctx, `SELECT `+sightingCols+`
		FROM sightings s JOIN restaurants r ON r.id = s.restaurant_id WHERE s.id = $1`, id)
	s, err := scanSighting(row)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// MarkSightingEnrolling moves a sighting into the enrolling state under a job id, recording
// who approved it. Only a ready/failed row can start: a second approve is a no-op.
func (d *DB) MarkSightingEnrolling(ctx context.Context, id uuid.UUID, jobID, approver string) (bool, error) {
	tag, err := d.pool.Exec(ctx, `
		UPDATE sightings SET state='enrolling', step=0, reason='', job_id=$2,
		    approved_by=$3, approved_at=NOW()
		WHERE id=$1 AND state IN ('ready','failed')`, id, jobID, approver)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// SetSightingStep records progress while a job runs.
func (d *DB) SetSightingStep(ctx context.Context, jobID string, step int) error {
	_, err := d.pool.Exec(ctx, `UPDATE sightings SET step=$2 WHERE job_id=$1 AND state='enrolling'`, jobID, step)
	return err
}

// FinishSighting ends a job: ok=true marks it enrolled (it leaves the inbox), ok=false
// returns it to failed with the reason so the row offers "Try again".
func (d *DB) FinishSighting(ctx context.Context, jobID string, ok bool, reason string) error {
	state := "failed"
	if ok {
		state = "enrolled"
	}
	_, err := d.pool.Exec(ctx, `UPDATE sightings SET state=$2, reason=$3, job_id='' WHERE job_id=$1`, jobID, state, reason)
	return err
}

// PruneStaleSightings removes ready/blocked/failed rows not seen for `age`. Enrolling and
// enrolled rows are left alone. Returns how many went.
func (d *DB) PruneStaleSightings(ctx context.Context, age time.Duration) (int64, error) {
	tag, err := d.pool.Exec(ctx, `
		DELETE FROM sightings WHERE state IN ('ready','blocked','failed')
		  AND last_seen < NOW() - make_interval(secs => $1)`, age.Seconds())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ScoutCandidate is a firmware device that could scout a restaurant.
type ScoutCandidate struct {
	ID     uuid.UUID
	Serial string
}

// ScoutCandidates lists the firmware devices in a restaurant, most recently seen first.
// The scout service picks the first one the hub reports connected.
func (d *DB) ScoutCandidates(ctx context.Context, restaurantID uuid.UUID) ([]ScoutCandidate, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, serial_number FROM devices
		WHERE restaurant_id = $1 AND agent_kind = 'firmware'
		  AND enrollment_status NOT IN ('retired','wiped','unenrolled')
		ORDER BY last_seen_at DESC`, restaurantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScoutCandidate
	for rows.Next() {
		var c ScoutCandidate
		if err := rows.Scan(&c.ID, &c.Serial); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RestaurantsToScan lists restaurants with scanning on that have at least one firmware
// device — the scheduler's work list.
func (d *DB) RestaurantsToScan(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT DISTINCT r.id FROM restaurants r
		JOIN devices d ON d.restaurant_id = r.id AND d.agent_kind = 'firmware'
		  AND d.enrollment_status NOT IN ('retired','wiped','unenrolled')
		WHERE r.scan_enabled`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// StampEnrolledViaScout fills in enrolled_via_serial from the sighting that approved
// this serial. The scout's own net_enroll_done also stamps it (SetEnrolledViaSerial),
// but that frame can arrive before the device's enrol POST has created the row — a
// brand-new serial, or one deleted for a re-test — and an UPDATE on a row that is not
// there yet is lost in silence. Calling this at enrol time closes that race from the
// other side; both are idempotent and neither overwrites an existing value.
func (d *DB) StampEnrolledViaScout(ctx context.Context, serial string) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE devices d SET enrolled_via_serial = s.scout_serial
		  FROM sightings s
		 WHERE d.serial_number = $1
		   AND s.serial = $1
		   AND s.scout_serial <> ''
		   AND s.state IN ('enrolling', 'enrolled')
		   AND COALESCE(d.enrolled_via_serial, '') = ''`, serial)
	return err
}

// DeviceKnown reports whether this server already manages a device with that serial —
// enrolled by any route, including the AIO Enroll desktop app. A scout probe cannot tell
// the difference on its own: a device already running our standard client answers with
// owner.ours = true, which classifySighting reads as "ready", because its blockers are
// all about somebody else's owner. So the server has to say so.
func (d *DB) DeviceKnown(ctx context.Context, serial string) (bool, error) {
	var ok bool
	err := d.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM devices
		  WHERE serial_number = $1 AND enrollment_status NOT IN ('retired', 'wiped', 'unenrolled'))`, serial).Scan(&ok)
	return ok, err
}

// MarkSightingOurs takes a serial out of the inbox because the device is now managed
// here. Called when any enrolment lands, so a device the desktop app enrolled stops
// being offered as something to enrol. An in-flight scout job is left alone — it
// finishes and sets its own state.
func (d *DB) MarkSightingOurs(ctx context.Context, serial string) error {
	if serial == "" {
		return nil
	}
	_, err := d.pool.Exec(ctx, `
		UPDATE sightings SET state = 'ours', reason = ''
		 WHERE serial = $1 AND state NOT IN ('enrolling', 'enrolled', 'ours')`, serial)
	return err
}

// SetEnrolledViaSerial stamps the scout that enrolled a device, once it has appeared.
func (d *DB) SetEnrolledViaSerial(ctx context.Context, serial, scoutSerial string) error {
	_, err := d.pool.Exec(ctx, `UPDATE devices SET enrolled_via_serial=$2 WHERE serial_number=$1`, serial, scoutSerial)
	return err
}

// SetRestaurantScan toggles the per-venue scout controls.
func (d *DB) SetRestaurantScan(ctx context.Context, id uuid.UUID, scanEnabled, autoEnroll bool) error {
	_, err := d.pool.Exec(ctx, `UPDATE restaurants SET scan_enabled=$2, auto_enroll=$3 WHERE id=$1`, id, scanEnabled, autoEnroll)
	return err
}

// DeviceScoutVenue returns a device's serial and the venue it is placed at, or a nil
// restaurant when it is unplaced. The scout needs both for every frame a device sends it,
// and GetDeviceByID does not select restaurant_id — a sighting ingested through that path
// looked like a bench unit and was dropped without a trace.
func (d *DB) DeviceScoutVenue(ctx context.Context, id uuid.UUID) (serial string, restaurantID *uuid.UUID, err error) {
	err = d.pool.QueryRow(ctx,
		`SELECT serial_number, restaurant_id FROM devices WHERE id = $1`, id).Scan(&serial, &restaurantID)
	return serial, restaurantID, err
}
