package db

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SQL for internal/cluster (scalability plan W5): several server copies sharing one
// fleet. See the cluster_* tables in migrations.go.

// ClusterAliveWindow is how recent a copy's heartbeat must be for its devices to count.
const ClusterAliveWindow = 30 * time.Second

// ClusterHeartbeat records that instance (reachable at addr) is up.
func (d *DB) ClusterHeartbeat(ctx context.Context, instance, addr string) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO cluster_instances (instance, addr) VALUES ($1, $2)
		ON CONFLICT (instance) DO UPDATE SET heartbeat_at = NOW(), addr = EXCLUDED.addr`, instance, addr)
	return err
}

// ClusterLeave removes instance and the devices it held, on a clean shutdown.
func (d *DB) ClusterLeave(ctx context.Context, instance string) error {
	if _, err := d.pool.Exec(ctx, `DELETE FROM device_owners WHERE instance = $1`, instance); err != nil {
		return err
	}
	_, err := d.pool.Exec(ctx, `DELETE FROM cluster_instances WHERE instance = $1`, instance)
	return err
}

// ClusterSetOwners records instance gaining (owned) and losing (lost) device sockets in
// one round trip. Losing only clears a row that is still this copy's: the device may
// already have reconnected to another copy.
func (d *DB) ClusterSetOwners(ctx context.Context, instance string, owned, lost []uuid.UUID) error {
	b := &pgx.Batch{}
	if len(owned) > 0 {
		b.Queue(`INSERT INTO device_owners (device_id, instance, at) SELECT unnest($1::uuid[]), $2, NOW()
			ON CONFLICT (device_id) DO UPDATE SET instance = EXCLUDED.instance, at = NOW()`, owned, instance)
	}
	if len(lost) > 0 {
		b.Queue(`DELETE FROM device_owners WHERE device_id = ANY($1) AND instance = $2`, lost, instance)
	}
	if b.Len() == 0 {
		return nil
	}
	return d.pool.SendBatch(ctx, b).Close()
}

// ClusterRemoteDevices is the devices held by live copies other than instance.
func (d *DB) ClusterRemoteDevices(ctx context.Context, instance string) (map[uuid.UUID]struct{}, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT o.device_id FROM device_owners o
		JOIN cluster_instances i ON i.instance = o.instance
		WHERE o.instance <> $1 AND i.heartbeat_at > NOW() - $2::interval`, instance, ClusterAliveWindow.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]struct{}{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

// ClusterNotify sends payload on channel to every copy listening (LISTEN).
func (d *DB) ClusterNotify(ctx context.Context, channel, payload string) error {
	_, err := d.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, channel, payload)
	return err
}

// ClusterPutMsg stores a payload too big for a NOTIFY and returns its id.
func (d *DB) ClusterPutMsg(ctx context.Context, payload []byte) (int64, error) {
	var id int64
	err := d.pool.QueryRow(ctx, `INSERT INTO cluster_msgs (payload) VALUES ($1) RETURNING id`, payload).Scan(&id)
	return id, err
}

// ClusterGetMsg reads a stored payload.
func (d *DB) ClusterGetMsg(ctx context.Context, id int64) ([]byte, error) {
	var p []byte
	err := d.pool.QueryRow(ctx, `SELECT payload FROM cluster_msgs WHERE id = $1`, id).Scan(&p)
	return p, err
}

// ClusterSweep drops stored payloads older than a few minutes, copies that stopped
// heartbeating an hour ago, and their device rows.
func (d *DB) ClusterSweep(ctx context.Context) error {
	if _, err := d.pool.Exec(ctx, `DELETE FROM cluster_msgs WHERE at < NOW() - INTERVAL '5 minutes'`); err != nil {
		return err
	}
	if _, err := d.pool.Exec(ctx, `
		DELETE FROM device_owners o USING cluster_instances i
		WHERE o.instance = i.instance AND i.heartbeat_at < NOW() - INTERVAL '1 hour'`); err != nil {
		return err
	}
	_, err := d.pool.Exec(ctx, `DELETE FROM cluster_instances WHERE heartbeat_at < NOW() - INTERVAL '1 hour'`)
	return err
}

// ListenConn opens a dedicated connection for LISTEN (outside the pool: it sits waiting
// for notifications for the life of the process).
func (d *DB) ListenConn(ctx context.Context) (*pgx.Conn, error) {
	return pgx.ConnectConfig(ctx, d.pool.Config().ConnConfig.Copy())
}
