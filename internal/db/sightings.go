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
	State           string // ready | blocked | ours
	Reason          string
}

// UpsertSighting records one device seen on a scan. It never overwrites a row that is
// mid-enrolment or already enrolled — a late scan frame must not undo a job in flight.
func (d *DB) UpsertSighting(ctx context.Context, s SightingUpsert) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO sightings (restaurant_id, scout_serial, host, port, serial, manufacturer, model,
		    android, owner_pkg, owner_ours, accounts, users, dpc_version, firmware_version,
		    class_guess, state, reason, last_seen)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,NOW())
		ON CONFLICT (restaurant_id, serial) DO UPDATE SET
		    scout_serial = EXCLUDED.scout_serial, host = EXCLUDED.host, port = EXCLUDED.port,
		    manufacturer = EXCLUDED.manufacturer, model = EXCLUDED.model, android = EXCLUDED.android,
		    owner_pkg = EXCLUDED.owner_pkg, owner_ours = EXCLUDED.owner_ours,
		    accounts = EXCLUDED.accounts, users = EXCLUDED.users,
		    dpc_version = EXCLUDED.dpc_version, firmware_version = EXCLUDED.firmware_version,
		    class_guess = EXCLUDED.class_guess, last_seen = NOW(),
		    state = CASE WHEN sightings.state IN ('enrolling','enrolled') THEN sightings.state ELSE EXCLUDED.state END,
		    reason = CASE WHEN sightings.state IN ('enrolling','enrolled') THEN sightings.reason ELSE EXCLUDED.reason END`,
		s.RestaurantID, s.ScoutSerial, s.Host, s.Port, s.Serial, s.Manufacturer, s.Model,
		s.Android, s.OwnerPkg, s.OwnerOurs, s.Accounts, s.Users, s.DPCVersion, s.FirmwareVersion,
		s.ClassGuess, s.State, s.Reason)
	return err
}

const sightingCols = `s.id, s.restaurant_id, COALESCE(r.name,''), s.scout_serial, s.host, s.port, s.serial,
	s.manufacturer, s.model, s.android, s.owner_pkg, s.owner_ours, s.accounts, s.users,
	s.dpc_version, s.firmware_version, s.class_guess, s.state, s.reason, s.step,
	s.approved_by, s.approved_at, s.job_id, s.first_seen, s.last_seen`

func scanSighting(row interface {
	Scan(dest ...any) error
}) (Sighting, error) {
	var s Sighting
	err := row.Scan(&s.ID, &s.RestaurantID, &s.RestaurantName, &s.ScoutSerial, &s.Host, &s.Port, &s.Serial,
		&s.Manufacturer, &s.Model, &s.Android, &s.OwnerPkg, &s.OwnerOurs, &s.Accounts, &s.Users,
		&s.DPCVersion, &s.FirmwareVersion, &s.ClassGuess, &s.State, &s.Reason, &s.Step,
		&s.ApprovedBy, &s.ApprovedAt, &s.JobID, &s.FirstSeen, &s.LastSeen)
	return s, err
}

// ListSightings returns every sighting not yet enrolled, newest venue activity first then
// ready devices first within a venue — the order the inbox card renders them. "ours"
// (firmware devices that enrol themselves) are excluded: nothing to do with them here.
func (d *DB) ListSightings(ctx context.Context) ([]Sighting, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT `+sightingCols+`
		FROM sightings s JOIN restaurants r ON r.id = s.restaurant_id
		WHERE s.state <> 'enrolled' AND s.state <> 'ours'
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
		  AND enrollment_status NOT IN ('retired','wiped')
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
		  AND d.enrollment_status NOT IN ('retired','wiped')
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
