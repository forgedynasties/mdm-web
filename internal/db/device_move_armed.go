package db

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Arming venues for the move to prod (19 Nov 2026; user: "the US venue devices that are
// waiting to be moved, can we hide all these venues and devices from mdm dev and arm it
// with a watcher, it waits for it to come online then makes sure mdm client is updated then
// move to prod", "so mdm dev only has aio lab venue devices"). An armed venue is hidden
// (restaurants.hidden: out of every list), its devices are hidden (devices.hidden, kept
// through check-ins by devices.arm_hidden), and each device has a move filed that the
// device-moves job carries out when the device is online and its MDM client is current.

// ArmDevice is one device of a venue that arming would take.
type ArmDevice struct {
	Serial     string     `json:"serial"`
	Kind       string     `json:"agent_kind"`
	Client     string     `json:"client_version"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
	MoveState  string     `json:"move_state,omitempty"` // "" = none filed
	DeviceID   uuid.UUID  `json:"-"`
}

// ArmVenue is a venue with the devices arming would take (not hidden, not retired).
type ArmVenue struct {
	ID      uuid.UUID   `json:"venue_id"`
	Name    string      `json:"venue"`
	Hidden  bool        `json:"hidden"`
	Devices []ArmDevice `json:"devices"`
}

// ArmCandidates lists the venues with devices still in view: the venue is not hidden yet,
// or it holds a visible device. only (when set) narrows to those venue ids.
func (d *DB) ArmCandidates(ctx context.Context, only []uuid.UUID) ([]ArmVenue, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT r.id, r.name, r.hidden, dv.id, dv.serial_number, dv.agent_kind,
		       COALESCE(dv.latest_extra->>'agent_version', ''), dv.last_seen_at, COALESCE(m.state, '')
		  FROM restaurants r
		  JOIN devices dv ON dv.restaurant_id = r.id AND NOT dv.hidden
		       AND dv.enrollment_status NOT IN ('retired', 'wiped', 'unenrolled')
		  LEFT JOIN device_moves m ON m.serial_number = dv.serial_number
		 WHERE (COALESCE(cardinality($1::uuid[]), 0) = 0 OR r.id = ANY($1))
		 ORDER BY r.name, r.id, dv.serial_number`, only)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ArmVenue
	for rows.Next() {
		var v ArmVenue
		var a ArmDevice
		if err := rows.Scan(&v.ID, &v.Name, &v.Hidden, &a.DeviceID, &a.Serial, &a.Kind, &a.Client, &a.LastSeenAt, &a.MoveState); err != nil {
			return nil, err
		}
		if n := len(out); n == 0 || out[n-1].ID != v.ID {
			out = append(out, v)
		}
		out[len(out)-1].Devices = append(out[len(out)-1].Devices, a)
	}
	return out, rows.Err()
}

// HideVenue hides a venue and the given devices of it (kept hidden through check-ins).
func (d *DB) HideVenue(ctx context.Context, venue uuid.UUID, serials []string) error {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE restaurants SET hidden = TRUE WHERE id = $1`, venue); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE devices SET hidden = TRUE, arm_hidden = TRUE
		WHERE restaurant_id = $1 AND serial_number = ANY($2)`, venue, serials); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// UnhideVenues reverses HideVenue for the venues: the venue and the devices arming hid come
// back. Returns how many devices.
func (d *DB) UnhideVenues(ctx context.Context, venues []uuid.UUID) (int, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE restaurants SET hidden = FALSE WHERE id = ANY($1)`, venues); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `UPDATE devices SET hidden = FALSE, arm_hidden = FALSE
		WHERE restaurant_id = ANY($1) AND arm_hidden`, venues)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), tx.Commit(ctx)
}

// HiddenVenueIDs lists the hidden venues (unarm all).
func (d *DB) HiddenVenueIDs(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := d.pool.Query(ctx, `SELECT id FROM restaurants WHERE hidden`)
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

// ClientStanding is where a device's MDM client stands against the build hosted for its
// slot (filled by the dashboard, which knows the hosted builds and the folding of slots).
type ClientStanding struct {
	State     string          `json:"state"` // current | behind | nobuild | unknown
	Slot      string          `json:"slot"`
	Have      string          `json:"have"`
	Want      string          `json:"want"`
	CanUpdate bool            `json:"can_update"` // the client can install a hosted build itself
	URL       string          `json:"-"`
	Payload   json.RawMessage `json:"-"`
}

// MoveUpdateBy is the sender the watcher's client updates carry.
const MoveUpdateBy = "move-update"

// MoveUpdateHistory is what the move watcher has asked of a device: when it last queued a
// client update and how many of those failed in the last day.
func (d *DB) MoveUpdateHistory(ctx context.Context, device uuid.UUID) (last time.Time, failed int, err error) {
	var l *time.Time
	err = d.pool.QueryRow(ctx, `
		SELECT MAX(c.created_at), COUNT(*) FILTER (WHERE cs.status = 'failed' AND c.created_at > NOW() - INTERVAL '24 hours')
		  FROM commands c
		  JOIN command_targets ct ON ct.command_id = c.id AND ct.target_id = $1
		  LEFT JOIN command_status cs ON cs.command_id = c.id AND cs.device_id = ct.target_id
		 WHERE c.type = 'app_update' AND c.created_by = $2`, device, MoveUpdateBy).Scan(&l, &failed)
	if l != nil {
		last = *l
	}
	return
}

// SameVenueName reports whether a venue name is in the list (case-insensitive).
func SameVenueName(name string, list []string) bool {
	for _, x := range list {
		if strings.EqualFold(strings.TrimSpace(x), strings.TrimSpace(name)) {
			return true
		}
	}
	return false
}
