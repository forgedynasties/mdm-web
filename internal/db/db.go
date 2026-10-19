package db

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type DB struct {
	pool *pgxpool.Pool

	// checkinSampleSec is the coalescing window for UpsertCheckin (see there). Set
	// from config at startup and whenever the setting is saved.
	checkinSampleSec atomic.Int32

	// flaps holds which devices' chargers are flapping, so history stores the flap as
	// one state instead of a row per toggle (see charger_flap.go).
	flaps flapTracker

	// nightly remembers which venues a once-a-night rule has checked (alert_nightly.go).
	nightly nightlyTracker

	// cmdSummaryMu/cmdSummaryCache short-TTL-cache GetCommandDeliverySummaries: it's
	// a full join+CASE-classify over the commands/command_status window, polled
	// every 20s by the Actions page from every open tab/browser. Collapsing repeat
	// calls within a few seconds of each other avoids redoing that join for every
	// concurrent viewer without serving meaningfully stale data.
	cmdSummaryMu    sync.Mutex
	cmdSummaryCache map[[2]int]cmdSummaryCacheEntry

	// roleApps caches role → app for the check-in path (role_model.go).
	roleApps roleAppCache

	// venueIcons caches which venues have an icon (restaurant_icon.go).
	venueIcons venueIconCache
}

type cmdSummaryCacheEntry struct {
	at   time.Time
	data map[uuid.UUID]CommandDeliverySummary
}

var ErrCommandNotTargeted = errors.New("command does not target device")

// ErrLogcatNotTargeted is returned when a device submits a logcat result for a
// request that was not issued to it.
var ErrLogcatNotTargeted = errors.New("logcat request does not target device")

func New(ctx context.Context, connStr string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		return nil, err
	}
	// The default max-conns (~4) starves the pool under concurrent device check-ins
	// plus dashboard traffic, serializing requests. Raise it unless the connection
	// string already specifies pool_max_conns.
	if cfg.MaxConns < 20 && !strings.Contains(connStr, "pool_max_conns") {
		// 25 was tight: the device-list page fans out ~12 concurrent queries, so a
		// couple of dashboards loading it plus check-in upserts could saturate the
		// pool. Since then, Overview (~8), Actions (~8), Fleet Wrapped (~15) and
		// DeviceDetail (~15) all gained the same concurrent-fan-out treatment, so a
		// few admins hitting different heavy pages at once can now approach the old
		// 40 headroom on top of steady check-in traffic. Raised further as a
		// preemptive margin.
		cfg.MaxConns = 60
	}
	cfg.MinConns = 2
	// Recycle connections so a long-lived pool rebalances after failovers/restarts;
	// jitter avoids all conns expiring at once (thundering-herd reconnect).
	//
	// The lifetime and idle time are what keep the pool's PEAK from becoming its
	// steady state, which on this database is the difference between running and being
	// OOM-killed. A Postgres backend does not return memory to the OS when its query
	// finishes — it keeps the context it allocated — so an idle connection still holds
	// whatever the largest query it ever ran needed. Measured on live: 59 idle
	// connections against a 2 GiB instance, 1.72 GiB of non-reclaimable anonymous
	// memory between them, roughly 29 MB apiece, and the cgroup sitting at 99.8% of its
	// limit with nothing running.
	//
	// So idle connections are reaped in a minute and a half rather than half an hour.
	// MaxConns stays at 60: the fan-out that needs it is real, and capping the burst
	// would trade a memory problem for a latency one. What was wrong was holding the
	// burst open long after it ended — between bursts the pool now falls back to
	// MinConns and gives the memory back.
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnLifetimeJitter = 5 * time.Minute
	cfg.MaxConnIdleTime = 90 * time.Second
	// Pin the session timezone to UTC so `::date` bucketing in the daily-stats
	// rollup is deterministic regardless of the server/container locale (and matches
	// the Go side, which uses time.Now().UTC()).
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["timezone"] = "UTC"
	// No JIT. It triggers on estimated cost, and our estimates run high on the wide
	// CTE queries: the daily rollup spent 3.7s of 9.7s compiling 202 functions to
	// process 28k rows. Nothing this server runs is big enough to win it back.
	cfg.ConnConfig.RuntimeParams["jit"] = "off"
	cfg.ConnConfig.Tracer = queryTracer{}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &DB{pool: pool}, nil
}

func (d *DB) Close() {
	d.pool.Close()
}

func (d *DB) Ping(ctx context.Context) error {
	return d.pool.Ping(ctx)
}

// PoolStats returns connection-pool metrics for observability (surfaced at
// /debug/vars). Pool saturation — AcquiredConns approaching MaxConns with a rising
// EmptyAcquireCount — is otherwise invisible and the most likely silent degradation.
func (d *DB) PoolStats() map[string]int64 {
	s := d.pool.Stat()
	return map[string]int64{
		"max_conns":           int64(s.MaxConns()),
		"total_conns":         int64(s.TotalConns()),
		"acquired_conns":      int64(s.AcquiredConns()),
		"idle_conns":          int64(s.IdleConns()),
		"empty_acquire_count": s.EmptyAcquireCount(),
		"acquire_count":       s.AcquireCount(),
		"canceled_acquire":    s.CanceledAcquireCount(),
	}
}

func (d *DB) RunMigrations(ctx context.Context) error {
	// Run on a single connection inside one transaction so SET LOCAL lock_timeout
	// scopes to the migration only. The lock_timeout makes a schema statement
	// fail fast rather than block indefinitely when another session holds a
	// conflicting lock — typically the previous container still draining during a
	// deploy. A fast failure exits the process, Docker (restart: unless-stopped)
	// retries, and by then the lock is gone; without it the migration hangs before
	// the server ever listens, which the proxy surfaces as a 502.
	conn, err := d.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '15s'"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, migrationSQL); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, legacyOTASchema); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, appFamilySchema); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, productNamesSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, roleModelSchema); err != nil {
		return err
	}
	if err := seedRoleApps(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type DBStats struct {
	Devices       int64 `json:"devices"`
	Checkins      int64 `json:"checkins"`
	Commands      int64 `json:"commands"`
	LogcatResults int64 `json:"logcat_results"`
}

func (d *DB) TableStats(ctx context.Context) (DBStats, error) {
	var s DBStats
	// Exact counts for the small tables; instant planner estimates (pg_class.reltuples,
	// maintained by autovacuum/ANALYZE) for the large high-churn ones — an exact
	// count(*) on device_samples/logcat_results full-scans millions of rows and was making
	// the Settings page take seconds to load. Estimates are fine for a stats readout.
	// Checkins counts device_samples: one row per stored report, the history the charts
	// read, now that the checkins table is being retired.
	err := d.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM devices),
		       (SELECT GREATEST(reltuples, 0)::bigint FROM pg_class WHERE oid = 'device_samples'::regclass),
		       (SELECT count(*) FROM commands),
		       (SELECT GREATEST(reltuples, 0)::bigint FROM pg_class WHERE oid = 'logcat_results'::regclass)
	`).Scan(&s.Devices, &s.Checkins, &s.Commands, &s.LogcatResults)
	return s, err
}
