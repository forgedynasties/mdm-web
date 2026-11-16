package db

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Moving devices to prod one by one (16 Nov 2026; user: "do this work for data without db
// queries but with api endpoints", "lets start venue by venue. for offline devices we can
// have the data imported and then wait for the devices to come back"). mdm.dev exports a
// device's bundle (its row and the settings kept beside it), prod imports it and waits for
// the device, and mdm.dev keeps a standing move in device_moves until the device has gone.
// History (readings, alerts, commands) stays on mdm.dev. Groups don't travel (user).
//
// Both sides lean on custody (custody.go): the imported row on prod carries custody
// "mdm.dev" (source migrate) so it raises no offline alerts and takes no commands until it
// checks in there, which clears it; mdm.dev files an arrived device as on prod (source
// migrate), with the same effect, until it checks in on mdm.dev again (it came back).

const deviceMovesSchema = `
CREATE TABLE IF NOT EXISTS device_moves (
	serial_number TEXT PRIMARY KEY,
	target        TEXT NOT NULL,
	state         TEXT NOT NULL DEFAULT 'pending', -- pending | sent | arrived | came_back | failed | canceled
	enroll_token  TEXT NOT NULL DEFAULT '',        -- Standard: the target's token for it (never listed)
	note          TEXT NOT NULL DEFAULT '',
	command_id    UUID,
	attempts      INT  NOT NULL DEFAULT 0,
	created_by    TEXT NOT NULL DEFAULT '',
	created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	imported_at   TIMESTAMPTZ,
	sent_at       TIMESTAMPTZ,
	arrived_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_device_moves_state ON device_moves(state);
`

// Move states.
const (
	MovePending  = "pending"
	MoveSent     = "sent"
	MoveArrived  = "arrived"
	MoveCameBack = "came_back"
	MoveFailed   = "failed"
	MoveCanceled = "canceled"
)

// CustodyMigrate is the custody source of a device moved to prod (or waiting on prod for
// one): it never goes stale by itself (ExpireStaleCustody), only a check-in clears it.
const CustodyMigrate = "migrate"

// moveTable is a table of per-device settings that travels with the device: its rows are
// the ones whose col names the device (its id, or its serial).
type moveTable struct {
	name, col string
	bySerial  bool
}

var moveTables = []moveTable{
	{name: "device_config", col: "device_id"},
	{name: "device_nicknames", col: "device_id"},
	{name: "device_app_follows", col: "device_id"},
	{name: "device_adb_key", col: "serial", bySerial: true},
	{name: "hardware_serials", col: "serial_number", bySerial: true},
}

// DeviceMoveBundle is one device as it travels: its devices row, the venue it is at (by id,
// as the venue sync copied them; the name only for messages) and its settings rows.
type DeviceMoveBundle struct {
	From   string                       `json:"from"`
	Serial string                       `json:"serial"`
	Venue  string                       `json:"venue,omitempty"`
	Device json.RawMessage              `json:"device"`
	Tables map[string][]json.RawMessage `json:"tables"`
}

// ExportDeviceMove reads one device's bundle. nil, nil when there is no such serial.
func (d *DB) ExportDeviceMove(ctx context.Context, serial, from string) (*DeviceMoveBundle, error) {
	b := &DeviceMoveBundle{From: from, Tables: map[string][]json.RawMessage{}}
	var id uuid.UUID
	var raw []byte
	err := d.pool.QueryRow(ctx, `
		SELECT dv.id, dv.serial_number, row_to_json(dv), COALESCE(r.name, '')
		  FROM devices dv LEFT JOIN restaurants r ON r.id = dv.restaurant_id
		 WHERE dv.serial_number = $1`, serial).Scan(&id, &b.Serial, &raw, &b.Venue)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b.Device = raw
	for _, t := range moveTables {
		var key any = id
		if t.bySerial {
			key = b.Serial
		}
		rows, err := d.pool.Query(ctx, fmt.Sprintf(`SELECT row_to_json(t) FROM %s t WHERE %s = $1`,
			pgx.Identifier{t.name}.Sanitize(), pgx.Identifier{t.col}.Sanitize()), key)
		if err != nil {
			return nil, fmt.Errorf("export %s: %w", t.name, err)
		}
		var out []json.RawMessage
		for rows.Next() {
			var r []byte
			if err := rows.Scan(&r); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, json.RawMessage(r))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		b.Tables[t.name] = out
	}
	return b, nil
}

// DeviceMoveImport is what an import did.
type DeviceMoveImport struct {
	Serial      string    `json:"serial"`
	DeviceID    uuid.UUID `json:"device_id"`
	Kind        string    `json:"agent_kind"`
	Created     bool      `json:"created"`
	EnrollToken string    `json:"enroll_token,omitempty"` // Standard: enrols it here
}

// ErrMoveVenueMissing: the device's venue is not on this server (sync venues first).
type ErrMoveVenueMissing struct{ Venue, ID string }

func (e ErrMoveVenueMissing) Error() string {
	return fmt.Sprintf("venue %q (%s) is not on this server: sync venues first", e.Venue, e.ID)
}

// ImportDeviceMove takes a bundle from the server the device is on now (fromName names it
// in custody, fromURL is its address). The device row is created or updated by serial and
// filed as still reporting there; its settings rows are replaced. A firmware device keeps
// its key fingerprint (its own key works here at once); a Standard device's is dropped,
// and it gets a token to enrol here with, for the venue it is at. newToken makes one.
func (d *DB) ImportDeviceMove(ctx context.Context, b *DeviceMoveBundle, fromName, fromURL string, newToken func() string) (*DeviceMoveImport, error) {
	var dev map[string]any
	if err := json.Unmarshal(b.Device, &dev); err != nil || dev == nil {
		return nil, fmt.Errorf("bad device row")
	}
	serial := strOf(dev["serial_number"])
	if serial == "" || serial != b.Serial {
		return nil, fmt.Errorf("the bundle's device has no serial or not its own")
	}
	kind := strOf(dev["agent_kind"])
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var venue *uuid.UUID
	if v := strOf(dev["restaurant_id"]); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return nil, fmt.Errorf("bad restaurant_id")
		}
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM restaurants WHERE id = $1)`, id).Scan(&ok); err != nil {
			return nil, err
		}
		if !ok {
			return nil, ErrMoveVenueMissing{Venue: b.Venue, ID: v}
		}
		venue = &id
	}

	// What changes on the way: the key (Standard enrols here), the token it came with
	// (that profile is on the other server), and custody (still reporting there).
	if kind != "firmware" {
		dev["device_key_hash"] = nil
	}
	dev["enrolled_via"] = nil
	seen := dev["last_seen_at"]
	if seen == nil {
		seen = time.Now().UTC().Format(time.RFC3339Nano)
	}
	dev["custody_server"], dev["custody_url"], dev["custody_seen_at"] = fromName, fromURL, seen
	dev["custody_source"], dev["custody_set_at"] = CustodyMigrate, time.Now().UTC().Format(time.RFC3339Nano)

	cols, err := tableColumns(ctx, tx, "devices")
	if err != nil {
		return nil, err
	}
	var ins, set []string
	for _, c := range cols {
		if _, ok := dev[c]; !ok {
			continue // a column the other server doesn't have keeps its default
		}
		q := pgx.Identifier{c}.Sanitize()
		ins = append(ins, q)
		if c != "id" && c != "serial_number" {
			set = append(set, q)
		}
	}
	exc := make([]string, len(set))
	for i, c := range set {
		exc[i] = "excluded." + c
	}
	raw, _ := json.Marshal(dev)
	res := &DeviceMoveImport{Serial: serial, Kind: kind}
	err = tx.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO devices (%[1]s) SELECT %[1]s FROM json_populate_record(NULL::devices, $1::json)
		ON CONFLICT (serial_number) DO UPDATE SET (%[2]s) = ROW(%[3]s)
		RETURNING id, (xmax = 0)`, strings.Join(ins, ", "), strings.Join(set, ", "), strings.Join(exc, ", ")),
		string(raw)).Scan(&res.DeviceID, &res.Created)
	if err != nil {
		return nil, fmt.Errorf("device: %w", err)
	}
	// The placement trigger stamps "placed now" on a venue change or a new row; the device
	// keeps the state it had (setting up, or in service since then).
	if _, err := tx.Exec(ctx, `UPDATE devices SET handed_over_at = $2::timestamptz, handed_over_by = $3 WHERE id = $1`,
		res.DeviceID, dev["handed_over_at"], strOf(dev["handed_over_by"])); err != nil {
		return nil, err
	}

	for _, t := range moveTables {
		rows, ok := b.Tables[t.name]
		if !ok {
			continue
		}
		var key any = res.DeviceID
		if t.bySerial {
			key = serial
		}
		tbl, col := pgx.Identifier{t.name}.Sanitize(), pgx.Identifier{t.col}.Sanitize()
		if _, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE %s = $1`, tbl, col), key); err != nil {
			return nil, fmt.Errorf("%s: %w", t.name, err)
		}
		for _, r := range rows {
			var obj map[string]any
			if err := json.Unmarshal(r, &obj); err != nil {
				return nil, fmt.Errorf("%s: bad row", t.name)
			}
			obj[t.col] = key
			if t.bySerial {
				obj[t.col] = serial
			}
			rr, _ := json.Marshal(obj)
			if _, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %[1]s SELECT * FROM json_populate_record(NULL::%[1]s, $1::json) ON CONFLICT DO NOTHING`, tbl), string(rr)); err != nil {
				return nil, fmt.Errorf("%s: %w", t.name, err)
			}
		}
	}

	if kind != "firmware" {
		// One profile per moved device, kept (never revoked: a Standard device that loses
		// its key gets back in with the token it enrolled with), re-armed on each import for
		// one more enrolment, at the device's venue so re-enrolling keeps it there.
		name := "Moved from " + fromName + " · " + serial
		err := tx.QueryRow(ctx, `
			UPDATE enrollment_profiles
			   SET restaurant_id = $2, max_enrolls = enroll_count + 1, expires_at = NOW() + interval '30 days'
			 WHERE name = $1 AND revoked_at IS NULL
			RETURNING token`, name, venue).Scan(&res.EnrollToken)
		if err == pgx.ErrNoRows {
			res.EnrollToken = newToken()
			_, err = tx.Exec(ctx, `
				INSERT INTO enrollment_profiles (name, token, notes, restaurant_id, expires_at, max_enrolls, created_by, placement)
				VALUES ($1, $2, $3, $4, NOW() + interval '30 days', 1, '', '')`,
				name, res.EnrollToken, "Made by the device move from "+fromURL+"; keep it: the device recovers a lost key with it", venue)
		}
		if err != nil {
			return nil, fmt.Errorf("enrollment profile: %w", err)
		}
	}
	return res, tx.Commit(ctx)
}

// DeviceMove is one row of device_moves, with what the list shows about the device.
type DeviceMove struct {
	Serial     string     `json:"serial"`
	Target     string     `json:"target"`
	State      string     `json:"state"`
	Note       string     `json:"note,omitempty"`
	Attempts   int        `json:"attempts"`
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	ImportedAt *time.Time `json:"imported_at,omitempty"`
	SentAt     *time.Time `json:"sent_at,omitempty"`
	ArrivedAt  *time.Time `json:"arrived_at,omitempty"`
	CommandID  *uuid.UUID `json:"command_id,omitempty"`
	// From the device row.
	DeviceID   uuid.UUID  `json:"device_id"`
	Kind       string     `json:"agent_kind"`
	VenueID    *uuid.UUID `json:"venue_id,omitempty"`
	Venue      string     `json:"venue,omitempty"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
	token      string
}

// Token is the Standard device's enrollment token on the target.
func (m DeviceMove) Token() string { return m.token }

const deviceMoveCols = `m.serial_number, m.target, m.state, m.note, m.attempts, m.created_by, m.created_at,
	m.updated_at, m.imported_at, m.sent_at, m.arrived_at, m.command_id, m.enroll_token,
	dv.id, dv.agent_kind, dv.restaurant_id, COALESCE(r.name, ''), dv.last_seen_at`

func scanDeviceMove(row interface{ Scan(...any) error }) (DeviceMove, error) {
	var m DeviceMove
	err := row.Scan(&m.Serial, &m.Target, &m.State, &m.Note, &m.Attempts, &m.CreatedBy, &m.CreatedAt,
		&m.UpdatedAt, &m.ImportedAt, &m.SentAt, &m.ArrivedAt, &m.CommandID, &m.token,
		&m.DeviceID, &m.Kind, &m.VenueID, &m.Venue, &m.LastSeenAt)
	return m, err
}

// ListDeviceMoves lists moves, newest first; states filters (” = all), venue too.
func (d *DB) ListDeviceMoves(ctx context.Context, states []string, venue *uuid.UUID) ([]DeviceMove, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT `+deviceMoveCols+`
		  FROM device_moves m
		  JOIN devices dv ON dv.serial_number = m.serial_number
		  LEFT JOIN restaurants r ON r.id = dv.restaurant_id
		 WHERE (COALESCE(cardinality($1::text[]), 0) = 0 OR m.state = ANY($1))
		   AND ($2::uuid IS NULL OR dv.restaurant_id = $2)
		 ORDER BY m.updated_at DESC`, states, venue)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceMove
	for rows.Next() {
		m, err := scanDeviceMove(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetDeviceMove reads one device's move (nil when it has none).
func (d *DB) GetDeviceMove(ctx context.Context, serial string) (*DeviceMove, error) {
	m, err := scanDeviceMove(d.pool.QueryRow(ctx, `
		SELECT `+deviceMoveCols+`
		  FROM device_moves m
		  JOIN devices dv ON dv.serial_number = m.serial_number
		  LEFT JOIN restaurants r ON r.id = dv.restaurant_id
		 WHERE m.serial_number = $1`, serial))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// PutDeviceMove files a move as pending (a new one, or one that ended: failed, came back,
// canceled), with the token the target answered the import with. A move under way (sent)
// or done (arrived) is left alone and reported false.
func (d *DB) PutDeviceMove(ctx context.Context, serial, target, token, by string) (bool, error) {
	tag, err := d.pool.Exec(ctx, `
		INSERT INTO device_moves (serial_number, target, enroll_token, created_by, imported_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (serial_number) DO UPDATE
		   SET target = EXCLUDED.target, state = 'pending', enroll_token = EXCLUDED.enroll_token,
		       created_by = EXCLUDED.created_by, created_at = NOW(), updated_at = NOW(),
		       imported_at = NOW(), sent_at = NULL, arrived_at = NULL, command_id = NULL,
		       note = '', attempts = 0
		 WHERE device_moves.state NOT IN ('sent', 'arrived')`, serial, target, token, by)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// MoveImported records a fresh import just before the move goes out (the token may be new).
func (d *DB) MoveImported(ctx context.Context, serial, token string) error {
	_, err := d.pool.Exec(ctx, `UPDATE device_moves SET imported_at = NOW(), enroll_token = $2, updated_at = NOW() WHERE serial_number = $1`, serial, token)
	return err
}

// SetDeviceMoveState moves a move on. sent stamps sent_at and the command and counts an
// attempt; arrived stamps arrived_at.
func (d *DB) SetDeviceMoveState(ctx context.Context, serial, state, note string, cmd *uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE device_moves
		   SET state = $2, note = $3, updated_at = NOW(),
		       command_id = COALESCE($4, command_id),
		       sent_at    = CASE WHEN $2 = 'sent' THEN NOW() ELSE sent_at END,
		       attempts   = attempts + CASE WHEN $2 = 'sent' THEN 1 ELSE 0 END,
		       arrived_at = CASE WHEN $2 = 'arrived' THEN NOW() ELSE arrived_at END
		 WHERE serial_number = $1`, serial, state, note, cmd)
	return err
}

// NoteDeviceMove sets a pending move's note (why it is still waiting), without touching
// updated_at, so the list keeps its order.
func (d *DB) NoteDeviceMove(ctx context.Context, serial, note string) error {
	_, err := d.pool.Exec(ctx, `UPDATE device_moves SET note = $2 WHERE serial_number = $1 AND note <> $2`, serial, note)
	return err
}

// CancelDeviceMove cancels a move not under way. Reports whether one was.
func (d *DB) CancelDeviceMove(ctx context.Context, serial string) (bool, error) {
	tag, err := d.pool.Exec(ctx, `
		UPDATE device_moves SET state = 'canceled', updated_at = NOW()
		 WHERE serial_number = $1 AND state IN ('pending', 'failed', 'came_back')`, serial)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// MoveCommandStatus is the move command's delivery to the device: its status ("" = not
// delivered yet) and output.
func (d *DB) MoveCommandStatus(ctx context.Context, cmd, device uuid.UUID) (status, output string, err error) {
	err = d.pool.QueryRow(ctx, `
		SELECT cs.status, COALESCE((SELECT output FROM command_results cr WHERE cr.command_id = cs.command_id AND cr.device_id = cs.device_id), '')
		  FROM command_status cs WHERE cs.command_id = $1 AND cs.device_id = $2`,
		cmd, device).Scan(&status, &output)
	if err == pgx.ErrNoRows {
		return "", "", nil
	}
	return status, output, err
}

// VenueDeviceSerials lists the serials of a venue's devices still in the fleet.
func (d *DB) VenueDeviceSerials(ctx context.Context, venue uuid.UUID) ([]string, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT serial_number FROM devices
		 WHERE restaurant_id = $1 AND NOT hidden
		   AND enrollment_status NOT IN ('retired', 'wiped', 'unenrolled')
		 ORDER BY serial_number`, venue)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
