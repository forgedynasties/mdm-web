package db

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// IslandJob is one command a person sent that is still running on some devices: the top
// island's small progress slot (D3, 3 Nov 2026).
type IslandJob struct {
	ID        uuid.UUID
	Type      string
	CreatedAt time.Time
	Total     int // devices it targets (named devices, or the members of its groups)
	Done      int // of those, finished one way or another
	Failed    int
}

// RunningJobsBy lists the commands created by user in the last six hours that still have a
// device working on them or waiting for them, newest first (at most limit). Fleet-wide
// ("all") commands are left out: their size is the whole fleet, not a job.
func (d *DB) RunningJobsBy(ctx context.Context, user string, limit int) ([]IslandJob, error) {
	rows, err := d.pool.Query(ctx, `
		WITH mine AS (
			SELECT c.id, c.type, c.created_at, c.target_type FROM commands c
			WHERE c.created_by = $1 AND c.created_at > NOW() - INTERVAL '6 hours'
			  AND c.target_type IN ('devices', 'groups')
		), targets AS (
			SELECT m.id, ct.target_id AS device_id FROM mine m JOIN command_targets ct ON ct.command_id = m.id WHERE m.target_type = 'devices'
			UNION
			SELECT m.id, dg.device_id FROM mine m JOIN command_targets ct ON ct.command_id = m.id
			  JOIN device_groups dg ON dg.group_id = ct.target_id WHERE m.target_type = 'groups'
		), agg AS (
			SELECT t.id, COUNT(*) AS total,
			       COUNT(*) FILTER (WHERE cs.status IN ('completed', 'installed', 'failed', 'cancelled', 'expired')) AS done,
			       COUNT(*) FILTER (WHERE cs.status = 'failed') AS failed
			FROM targets t LEFT JOIN command_status cs ON cs.command_id = t.id AND cs.device_id = t.device_id
			GROUP BY t.id
		)
		SELECT m.id, m.type, m.created_at, a.total, a.done, a.failed
		FROM mine m JOIN agg a ON a.id = m.id
		WHERE a.done < a.total
		ORDER BY m.created_at DESC LIMIT $2
	`, user, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IslandJob
	for rows.Next() {
		var j IslandJob
		if err := rows.Scan(&j.ID, &j.Type, &j.CreatedAt, &j.Total, &j.Done, &j.Failed); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
