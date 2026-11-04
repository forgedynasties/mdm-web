package db

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Custody answers "where is this device reporting?" for a device that is not reporting
// here. A device moves between MDM servers by having persist.sys.mdm.url changed — from
// its device page, by hand over adb, or by flashing an image whose build.prop already
// carries the property — and the server it leaves sees only silence, which is exactly
// what a dead device looks like. Guessing is not good enough: an operator acts on the
// difference (chase the hardware, or leave it alone), and so does this server, which
// otherwise keeps queueing commands and updates nothing will ever collect.
//
// The rules, in one place:
//
//   - Custody is only ever set from evidence that names a time: a peer saying it saw
//     the device at T. A move command is a strong hint, so it is recorded too, but it
//     is marked as such (source "move") and does not claim the device arrived.
//   - A check-in here clears custody, in the same statement that records it.
//   - The newest sighting wins. A peer reporting a sighting older than our own
//     last_seen is ignored: we heard from it more recently than they did.
type Custody struct {
	Server  string    // peer name ("stage"); "" = this server
	URL     string    // peer base URL
	SeenAt  time.Time // the peer's last_seen for it; zero when only suspected
	Source  string    // move | peer | sweep | client
	SetAt   time.Time
	Suspect bool // recorded from a move command, never confirmed by a peer
}

// Elsewhere reports whether the device is believed to be on another server at all.
func (c Custody) Elsewhere() bool { return c.Server != "" }

// CustodyArrivalGap is how long a device must have been silent for its next check-in to
// count as an arrival worth announcing to peers. Long enough that a device cycling
// through the night does not announce every morning; short enough that a device moved
// and back within a day is still reported.
const CustodyArrivalGap = 6 * time.Hour

// CustodyStaleAfter is how long a peer sighting is trusted. Past this, a device is not
// reporting anywhere, so it stops being "on stage" and becomes plainly missing —
// otherwise a device that dies on the other server hides behind the label forever.
const CustodyStaleAfter = 7 * 24 * time.Hour

// SetDeviceCustody files a device as reporting to another server. seenAt is the peer's
// own last_seen; pass the zero time for a suspicion (a move we issued but nobody has
// confirmed). Returns false when the claim was ignored because we have heard from the
// device more recently than the peer has.
func (d *DB) SetDeviceCustody(ctx context.Context, deviceID uuid.UUID, server, url string, seenAt time.Time, source string) (bool, error) {
	var seen *time.Time
	if !seenAt.IsZero() {
		seen = &seenAt
	}
	tag, err := d.pool.Exec(ctx, `
		UPDATE devices
		   SET custody_server = $2, custody_url = $3, custody_seen_at = $4,
		       custody_source = $5, custody_set_at = NOW()
		 WHERE id = $1
		   -- Only when the peer's sighting is newer than our own. Two servers that both
		   -- hear from a device (a flapping move, a cloned serial) must not overwrite
		   -- each other in a loop; the later sighting is the true one.
		   AND ($4::timestamptz IS NULL OR $4 > devices.last_seen_at)
		   AND ($4::timestamptz IS NULL OR devices.custody_seen_at IS NULL OR $4 >= devices.custody_seen_at)`,
		deviceID, server, url, seen, source)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ClearDeviceCustody files a device as ours again. Called when it checks in (the upsert
// does it inline) and when an operator says it is back.
func (d *DB) ClearDeviceCustody(ctx context.Context, deviceID uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE devices SET custody_server = '', custody_url = '', custody_seen_at = NULL,
		       custody_source = '', custody_set_at = NULL
		 WHERE id = $1 AND custody_server <> ''`, deviceID)
	return err
}

// DeviceCustody reads one device's custody.
func (d *DB) DeviceCustody(ctx context.Context, deviceID uuid.UUID) (Custody, error) {
	var c Custody
	var seen, set *time.Time
	err := d.pool.QueryRow(ctx, `
		SELECT custody_server, custody_url, custody_seen_at, custody_source, custody_set_at
		  FROM devices WHERE id = $1`, deviceID).Scan(&c.Server, &c.URL, &seen, &c.Source, &set)
	if err == pgx.ErrNoRows {
		return Custody{}, nil
	}
	if err != nil {
		return Custody{}, err
	}
	if seen != nil {
		c.SeenAt = *seen
	}
	if set != nil {
		c.SetAt = *set
	}
	c.Suspect = c.Server != "" && seen == nil
	return c, nil
}

// DeviceCustodyMap is the list-page form: custody for many devices in one query, keyed
// by device id, with only the devices that are actually elsewhere present.
func (d *DB) DeviceCustodyMap(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Custody, error) {
	out := map[uuid.UUID]Custody{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT id, custody_server, custody_url, custody_seen_at, custody_source, custody_set_at
		  FROM devices WHERE id = ANY($1) AND custody_server <> ''`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var c Custody
		var seen, set *time.Time
		if err := rows.Scan(&id, &c.Server, &c.URL, &seen, &c.Source, &set); err != nil {
			return nil, err
		}
		if seen != nil {
			c.SeenAt = *seen
		}
		if set != nil {
			c.SetAt = *set
		}
		c.Suspect = seen == nil
		out[id] = c
	}
	return out, rows.Err()
}

// ExpireStaleCustody drops custody claims nobody has refreshed. The device is then
// plainly missing again — which is the truth: it is not reporting anywhere.
func (d *DB) ExpireStaleCustody(ctx context.Context) (int64, error) {
	tag, err := d.pool.Exec(ctx, `
		UPDATE devices SET custody_server = '', custody_url = '', custody_seen_at = NULL,
		       custody_source = '', custody_set_at = NULL
		 WHERE custody_server <> ''
		   AND COALESCE(custody_seen_at, custody_set_at) < NOW() - $1::interval`, CustodyStaleAfter)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// CancelPendingForCustody clears out what a device can no longer collect, the moment we
// learn it is reporting elsewhere: queued command targets it will never fetch, and any
// deployment membership that would otherwise wait on it forever. Commands already
// delivered keep their status — that happened, and the record should say so.
func (d *DB) CancelPendingForCustody(ctx context.Context, deviceID uuid.UUID) (int64, error) {
	tag, err := d.pool.Exec(ctx, `
		DELETE FROM command_targets ct
		 WHERE ct.target_id = $1
		   AND NOT EXISTS (SELECT 1 FROM command_status cs
		                    WHERE cs.command_id = ct.command_id AND cs.device_id = $1)`, deviceID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ServerWorkCounts is the Server page's "work in flight" block in one round trip. One
// query rather than seven: it runs every two seconds for every operator with the page
// open, and seven round trips each tick is exactly the kind of self-inflicted load a
// metrics page should not add.
//
// One query also means one failure: a single bad table name zeroes every count. It
// did — `deployments` had become `updates` and the block read all zeroes, silently,
// because the error was dropped. So it is returned now.
func (d *DB) ServerWorkCounts(ctx context.Context) (commandsPending, deploymentsLive, otaInFlight, devicesTotal, devicesElsewhere, alertsOpen int, err error) {
	err = d.pool.QueryRow(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM command_targets ct
		     JOIN commands c ON c.id = ct.command_id
		    WHERE c.created_at > NOW() - INTERVAL '24 hours'
		      AND NOT EXISTS (SELECT 1 FROM command_status cs
		                       WHERE cs.command_id = ct.command_id AND cs.device_id = ct.target_id
		                         AND cs.status IN ('completed', 'failed', 'expired'))),
		  (SELECT COUNT(*) FROM updates WHERE status = 'active'),
		  -- Per-device OTA progress lives on update_devices; updates.status is the
		  -- deployment's own state (pending / active / complete / canceled).
		  (SELECT COUNT(*) FROM update_devices ud JOIN updates u ON u.id = ud.update_id
		    WHERE u.status = 'active' AND ud.status IN ('downloading', 'installing', 'verifying', 'finalizing')),
		  (SELECT COUNT(*) FROM devices WHERE NOT hidden AND enrollment_status NOT IN ('retired', 'wiped', 'unenrolled')),
		  (SELECT COUNT(*) FROM devices WHERE custody_server <> ''),
		  (SELECT COUNT(*) FROM alerts WHERE status <> 'resolved')
	`).Scan(&commandsPending, &deploymentsLive, &otaInFlight, &devicesTotal, &devicesElsewhere, &alertsOpen)
	return
}

// ── Server page: database vitals ──────────────────────────────────────────────

// PGStats is what Postgres says about itself: size, cache behaviour, who is connected
// and what the biggest tables are. Read on a timer well above the page's refresh rate
// (see the cache in the dashboard handler) because these read catalog views and, for
// the table sizes, hit the filesystem — cheap once a minute, not cheap every two
// seconds with several operators watching.
type PGStats struct {
	SizeMB          float64     `json:"size_mb"`
	CacheHitPct     float64     `json:"cache_hit_pct"`
	Commits         int64       `json:"commits"`
	Rollbacks       int64       `json:"rollbacks"`
	Deadlocks       int64       `json:"deadlocks"`
	TempFiles       int64       `json:"temp_files"`
	ConnActive      int         `json:"conn_active"`
	ConnIdle        int         `json:"conn_idle"`
	ConnIdleTx      int         `json:"conn_idle_tx"`
	ConnTotal       int         `json:"conn_total"`
	ConnMax         int         `json:"conn_max"`
	LongestQuerySec float64     `json:"longest_query_sec"`
	Tables          []TableStat `json:"tables"`
}

// TableStat is one table's footprint. Rows are the planner's estimate (reltuples), not
// a COUNT(*): an exact count of a check-in table with millions of rows would be the
// most expensive thing on the page by a wide margin, and the estimate answers the
// question being asked — is this table growing?
type TableStat struct {
	Name    string  `json:"name"`
	SizeMB  float64 `json:"size_mb"`
	Rows    int64   `json:"rows"`
	DeadPct float64 `json:"dead_pct"` // dead tuples as a share — bloat, i.e. vacuum pressure
}

// ServerPGStats gathers the database vitals in two queries.
func (d *DB) ServerPGStats(ctx context.Context) (PGStats, error) {
	var s PGStats
	err := d.pool.QueryRow(ctx, `
		SELECT
		  pg_database_size(current_database()) / 1048576.0,
		  COALESCE((SELECT CASE WHEN blks_hit + blks_read = 0 THEN 100
		                        ELSE blks_hit * 100.0 / (blks_hit + blks_read) END
		              FROM pg_stat_database WHERE datname = current_database()), 100),
		  COALESCE((SELECT xact_commit   FROM pg_stat_database WHERE datname = current_database()), 0),
		  COALESCE((SELECT xact_rollback FROM pg_stat_database WHERE datname = current_database()), 0),
		  COALESCE((SELECT deadlocks     FROM pg_stat_database WHERE datname = current_database()), 0),
		  COALESCE((SELECT temp_files    FROM pg_stat_database WHERE datname = current_database()), 0),
		  (SELECT COUNT(*) FROM pg_stat_activity WHERE state = 'active'),
		  (SELECT COUNT(*) FROM pg_stat_activity WHERE state = 'idle'),
		  (SELECT COUNT(*) FROM pg_stat_activity WHERE state = 'idle in transaction'),
		  (SELECT COUNT(*) FROM pg_stat_activity),
		  COALESCE((SELECT setting::int FROM pg_settings WHERE name = 'max_connections'), 0),
		  -- The oldest query still running, ignoring this one. A number that climbs
		  -- here is the shape of a lock wait or a runaway report.
		  COALESCE((SELECT EXTRACT(EPOCH FROM (NOW() - MIN(query_start)))
		              FROM pg_stat_activity
		             WHERE state = 'active' AND pid <> pg_backend_pid()), 0)
	`).Scan(&s.SizeMB, &s.CacheHitPct, &s.Commits, &s.Rollbacks, &s.Deadlocks, &s.TempFiles,
		&s.ConnActive, &s.ConnIdle, &s.ConnIdleTx, &s.ConnTotal, &s.ConnMax, &s.LongestQuerySec)
	if err != nil {
		return s, err
	}

	rows, err := d.pool.Query(ctx, `
		SELECT c.relname,
		       pg_total_relation_size(c.oid) / 1048576.0,
		       GREATEST(c.reltuples, 0)::bigint,
		       CASE WHEN COALESCE(st.n_live_tup, 0) + COALESCE(st.n_dead_tup, 0) = 0 THEN 0
		            ELSE COALESCE(st.n_dead_tup, 0) * 100.0 / (COALESCE(st.n_live_tup, 0) + COALESCE(st.n_dead_tup, 0)) END
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		  LEFT JOIN pg_stat_user_tables st ON st.relid = c.oid
		 WHERE c.relkind = 'r' AND n.nspname = 'public'
		 ORDER BY pg_total_relation_size(c.oid) DESC
		 LIMIT 8`)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var t TableStat
		if err := rows.Scan(&t.Name, &t.SizeMB, &t.Rows, &t.DeadPct); err != nil {
			return s, err
		}
		s.Tables = append(s.Tables, t)
	}
	return s, rows.Err()
}

// DischargePoint is one day's cumulative battery wear for a device: how many percent of
// pack capacity it had discharged, in total, by the end of that day.
type DischargePoint struct {
	Day time.Time `json:"d"`
	Cum int64     `json:"c"`
}

// DeviceDischargeSeries is lifetime wear over time, so the battery graph can answer
// "how many cycles had this pack done by this point?" — the same measure as the
// "Battery cycles" figure on the device page (discharge ÷ 100), not the count of
// 95%→10% excursions visible in the plotted window. The two were never the same number,
// and a readout that says 0 next to a card that says 14 is the graph's fault, not the
// operator's.
//
// Daily granularity because that is where the data lives: per-day discharge is rolled
// up in device_daily_stats, and per-check-in deltas are too noisy to sum into a
// trustworthy total.
func (d *DB) DeviceDischargeSeries(ctx context.Context, deviceID uuid.UUID) ([]DischargePoint, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT day,
		       SUM(COALESCE(discharge_pct, GREATEST(0, battery_max - battery_min)))
		           OVER (ORDER BY day)::bigint AS cum
		  FROM device_daily_stats
		 WHERE device_id = $1 AND battery_max IS NOT NULL AND battery_min IS NOT NULL
		 ORDER BY day`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DischargePoint
	for rows.Next() {
		var p DischargePoint
		if err := rows.Scan(&p.Day, &p.Cum); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
