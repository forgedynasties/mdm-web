package db

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

var hwSerialRe = regexp.MustCompile(`^[0-9A-F]{4,16}$`)

// NormalizeHardwareSerial returns the chip serial as upper-case hex, or "" when what a client
// sent is not one (empty, too short or long, not hex, or all zeros).
func NormalizeHardwareSerial(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "0X")
	if !hwSerialRe.MatchString(s) || strings.Trim(s, "0") == "" {
		return ""
	}
	return s
}

// HardwareSerialEvent says what a report of the chip serial changed.
type HardwareSerialEvent struct {
	Kind     string   // "" nothing new, "first" first report, "board_changed", "serial_changed"
	Previous string   // the chip serial this device reported before (board_changed)
	Others   []string // other AIO serials this chip serial has been seen under (serial_changed)
}

// RecordHardwareSerial notes that the device with this AIO serial reports this chip serial.
// It never refuses anything; it only says whether the report is worth an alert:
//   - board_changed: this AIO serial used to report a different chip serial (a swapped board,
//     or a serial that moved to another unit);
//   - serial_changed: this chip serial has been seen under another AIO serial, i.e. the same
//     unit now calls itself something else (a corrupted or rewritten serial), or two units
//     share a chip serial.
func (d *DB) RecordHardwareSerial(ctx context.Context, deviceID uuid.UUID, serial, hw string) (HardwareSerialEvent, error) {
	var ev HardwareSerialEvent
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return ev, err
	}
	defer tx.Rollback(ctx)

	var inserted bool
	if err := tx.QueryRow(ctx, `
		INSERT INTO hardware_serials (hardware_serial, serial_number) VALUES ($1, $2)
		ON CONFLICT (hardware_serial, serial_number) DO UPDATE SET last_seen_at = NOW()
		RETURNING (xmax = 0)`, hw, serial).Scan(&inserted); err != nil {
		return ev, err
	}
	var prev *string
	if err := tx.QueryRow(ctx, `SELECT hardware_serial FROM devices WHERE id = $1 FOR UPDATE`, deviceID).Scan(&prev); err != nil {
		return ev, err
	}
	if prev == nil || *prev != hw {
		if _, err := tx.Exec(ctx, `UPDATE devices SET hardware_serial = $2 WHERE id = $1`, deviceID, hw); err != nil {
			return ev, err
		}
	}
	rows, err := tx.Query(ctx, `
		SELECT serial_number FROM hardware_serials
		WHERE hardware_serial = $1 AND serial_number <> $2 ORDER BY last_seen_at DESC LIMIT 5`, hw, serial)
	if err != nil {
		return ev, err
	}
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			rows.Close()
			return ev, err
		}
		ev.Others = append(ev.Others, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ev, err
	}
	switch {
	case prev != nil && *prev != "" && *prev != hw:
		ev.Kind, ev.Previous = "board_changed", *prev
	case inserted && len(ev.Others) > 0:
		ev.Kind = "serial_changed"
	case inserted && prev == nil:
		ev.Kind = "first"
	}
	return ev, tx.Commit(ctx)
}

// HardwareSerialRow is one line of the map from a chip serial to an AIO serial.
type HardwareSerialRow struct {
	HardwareSerial string    `json:"hardware_serial"`
	SerialNumber   string    `json:"serial_number"`
	FirstSeenAt    time.Time `json:"first_seen_at"`
	LastSeenAt     time.Time `json:"last_seen_at"`
	Current        bool      `json:"current"` // the device with this AIO serial reports this chip serial now
}

// ListHardwareSerials returns the map, newest first. hw and serial narrow it when non-empty.
func (d *DB) ListHardwareSerials(ctx context.Context, hw, serial string) ([]HardwareSerialRow, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT h.hardware_serial, h.serial_number, h.first_seen_at, h.last_seen_at,
		       COALESCE(dv.hardware_serial = h.hardware_serial, false)
		FROM hardware_serials h
		LEFT JOIN devices dv ON dv.serial_number = h.serial_number
		WHERE ($1 = '' OR h.hardware_serial = $1) AND ($2 = '' OR h.serial_number = $2)
		ORDER BY h.last_seen_at DESC, h.serial_number
		LIMIT 5000`, hw, serial)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HardwareSerialRow{}
	for rows.Next() {
		var r HardwareSerialRow
		if err := rows.Scan(&r.HardwareSerial, &r.SerialNumber, &r.FirstSeenAt, &r.LastSeenAt, &r.Current); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// HardwareSerialOf returns the chip serial a device last reported, "" if it never has.
func (d *DB) HardwareSerialOf(ctx context.Context, serial string) string {
	var hw *string
	if err := d.pool.QueryRow(ctx, `SELECT hardware_serial FROM devices WHERE serial_number = $1`, serial).Scan(&hw); err != nil || hw == nil {
		return ""
	}
	return *hw
}
