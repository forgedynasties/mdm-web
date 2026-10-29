package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// TryJobLease claims job name for holder until now+hold, if nobody else holds it (scale
// plan W2b): with two server copies each job still runs once a tick. A lease is held for
// the length of the run (hold = the job's timeout) and shortened by ReleaseJobLease when
// it ends, so a copy that dies mid-run frees it at the timeout at the latest.
func (d *DB) TryJobLease(ctx context.Context, name, holder string, hold time.Duration) (bool, error) {
	var got string
	err := d.pool.QueryRow(ctx, `
		INSERT INTO job_leases (name, holder, until) VALUES ($1, $2, NOW() + $3::interval)
		ON CONFLICT (name) DO UPDATE SET holder = EXCLUDED.holder, until = EXCLUDED.until
		WHERE job_leases.until < NOW() OR job_leases.holder = EXCLUDED.holder
		RETURNING name`, name, holder, hold.String()).Scan(&got)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ReleaseJobLease ends holder's lease on name at next: just before the job's next tick,
// so another copy can take that tick but none can run this one twice.
func (d *DB) ReleaseJobLease(ctx context.Context, name, holder string, next time.Time) error {
	_, err := d.pool.Exec(ctx, `UPDATE job_leases SET until = $3 WHERE name = $1 AND holder = $2`, name, holder, next)
	return err
}
