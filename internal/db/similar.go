package db

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// SerialCandidate is an enrolled device whose serial might be the one someone meant.
type SerialCandidate struct {
	ID         uuid.UUID
	Serial     string
	Restaurant string
	LastSeen   *time.Time
}

// SerialCandidates returns devices whose serial could be a near miss for serial: it
// contains it or is contained by it (a cut-off paste), or shares its first six
// characters (a typo further in), case-insensitively. It only gathers candidates; the
// dashboard ranks them by edit distance and keeps the close ones. Runs only when a
// device page 404s.
func (d *DB) SerialCandidates(ctx context.Context, serial string, limit int) ([]SerialCandidate, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT d.id, d.serial_number, COALESCE(r.name, ''), d.last_seen_at
		FROM devices d LEFT JOIN restaurants r ON r.id = d.restaurant_id
		WHERE upper(d.serial_number) LIKE '%' || upper($1) || '%'
		   OR upper($1) LIKE '%' || upper(d.serial_number) || '%'
		   OR left(upper(d.serial_number), 6) = left(upper($1), 6)
		ORDER BY d.last_seen_at DESC NULLS LAST LIMIT $2`, serial, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SerialCandidate
	for rows.Next() {
		var c SerialCandidate
		if err := rows.Scan(&c.ID, &c.Serial, &c.Restaurant, &c.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
