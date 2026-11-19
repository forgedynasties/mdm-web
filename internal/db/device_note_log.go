package db

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Device notes are a log (18 Nov 2026, user: "notes can have a log with each timestamp, keep
// logging stuff"): every entry has a time, an author and a kind, new ones go on top and old
// ones stay. The single free-text note devices.notes is kept as the newest entry's text, so
// everything that reads it (the dot on the dock, the classic page) still works; the table is
// filled from it once, so no note is lost. What the MDM did to the device (moves, role and
// lock changes, key resets…) is not copied here: it comes from the audit log at read time.

const deviceNoteLogSchema = `
CREATE TABLE IF NOT EXISTS device_note_entries (
	id        BIGSERIAL PRIMARY KEY,
	device_id UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	author    TEXT NOT NULL DEFAULT '',
	kind      TEXT NOT NULL DEFAULT 'note' CHECK (kind IN ('note', 'issue', 'fix', 'hardware')),
	body      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_device_note_entries_device ON device_note_entries (device_id, at DESC);
-- The notes devices already have become their first entry (once per device).
INSERT INTO device_note_entries (device_id, at, author, kind, body)
SELECT d.id, NOW(), '', 'note', d.notes FROM devices d
WHERE d.notes <> '' AND NOT EXISTS (SELECT 1 FROM device_note_entries e WHERE e.device_id = d.id);
`

// DeviceNoteEntry is one line of a device's notes log.
type DeviceNoteEntry struct {
	ID     int64
	At     time.Time
	Author string
	Kind   string
	Body   string
}

// NoteKinds are the kinds an entry may have.
var NoteKinds = []string{"note", "issue", "fix", "hardware"}

// AddDeviceNote appends an entry and keeps devices.notes at the newest entry's text.
func (d *DB) AddDeviceNote(ctx context.Context, deviceID uuid.UUID, author, kind, body string) error {
	if _, err := d.pool.Exec(ctx, `INSERT INTO device_note_entries (device_id, author, kind, body) VALUES ($1, $2, $3, $4)`,
		deviceID, author, kind, body); err != nil {
		return err
	}
	_, err := d.pool.Exec(ctx, `UPDATE devices SET notes = $2 WHERE id = $1`, deviceID, body)
	return err
}

// ListDeviceNotes returns a device's notes, newest first.
func (d *DB) ListDeviceNotes(ctx context.Context, deviceID uuid.UUID, limit int) ([]DeviceNoteEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := d.pool.Query(ctx, `SELECT id, at, author, kind, body FROM device_note_entries WHERE device_id = $1 ORDER BY at DESC, id DESC LIMIT $2`, deviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceNoteEntry
	for rows.Next() {
		var e DeviceNoteEntry
		if err := rows.Scan(&e.ID, &e.At, &e.Author, &e.Kind, &e.Body); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
