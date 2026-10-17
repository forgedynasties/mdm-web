package db

import (
	"context"
	"time"
)

// DeviceQuery is one admin-curated diagnostic: a friendly label + the shell command
// it runs. Surfaced on the device page as a "retrieve property" button. The command
// text originates only here (never user input), so lower roles may run it safely.
type DeviceQuery struct {
	ID          int
	Label       string
	Description string
	Command     string
	Category    string
	Sort        int
	Enabled     bool
	CreatedBy   string
	CreatedAt   time.Time
}

const deviceQueryCols = `id, label, description, command, category, sort, enabled, created_by, created_at`

func scanDeviceQuery(row interface{ Scan(...any) error }, q *DeviceQuery) error {
	return row.Scan(&q.ID, &q.Label, &q.Description, &q.Command, &q.Category, &q.Sort, &q.Enabled, &q.CreatedBy, &q.CreatedAt)
}

// ListDeviceQueries returns the whole catalog (admin view), ordered for display.
func (d *DB) ListDeviceQueries(ctx context.Context) ([]DeviceQuery, error) {
	rows, err := d.pool.Query(ctx, `SELECT `+deviceQueryCols+` FROM device_queries ORDER BY category, sort, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceQuery
	for rows.Next() {
		var q DeviceQuery
		if err := scanDeviceQuery(rows, &q); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// ListEnabledDeviceQueries returns only enabled queries — what a run panel offers.
func (d *DB) ListEnabledDeviceQueries(ctx context.Context) ([]DeviceQuery, error) {
	all, err := d.ListDeviceQueries(ctx)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, q := range all {
		if q.Enabled {
			out = append(out, q)
		}
	}
	return out, nil
}

// GetDeviceQuery returns one query by id.
func (d *DB) GetDeviceQuery(ctx context.Context, id int) (DeviceQuery, error) {
	var q DeviceQuery
	err := scanDeviceQuery(d.pool.QueryRow(ctx, `SELECT `+deviceQueryCols+` FROM device_queries WHERE id = $1`, id), &q)
	return q, err
}

// CreateDeviceQuery inserts a new catalog entry and returns its id.
func (d *DB) CreateDeviceQuery(ctx context.Context, q DeviceQuery) (int, error) {
	var id int
	err := d.pool.QueryRow(ctx, `
		INSERT INTO device_queries (label, description, command, category, sort, enabled, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		q.Label, q.Description, q.Command, q.Category, q.Sort, q.Enabled, q.CreatedBy).Scan(&id)
	return id, err
}

// UpdateDeviceQuery edits an existing catalog entry.
func (d *DB) UpdateDeviceQuery(ctx context.Context, q DeviceQuery) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE device_queries SET label=$1, description=$2, command=$3, category=$4, sort=$5, enabled=$6 WHERE id=$7`,
		q.Label, q.Description, q.Command, q.Category, q.Sort, q.Enabled, q.ID)
	return err
}

// SetDeviceQueryEnabled flips a query's enabled flag.
func (d *DB) SetDeviceQueryEnabled(ctx context.Context, id int, enabled bool) error {
	_, err := d.pool.Exec(ctx, `UPDATE device_queries SET enabled=$1 WHERE id=$2`, enabled, id)
	return err
}

// DeleteDeviceQuery removes a catalog entry.
func (d *DB) DeleteDeviceQuery(ctx context.Context, id int) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM device_queries WHERE id = $1`, id)
	return err
}
