package db

// Telemetry tables split into monthly partitions (scalability plan W3, 29 Oct 2026).
//
// device_samples, battery_samples and app_usage were plain tables trimmed with DELETE:
// at 5,000 firmware devices that is ~18 M rows a day to insert and later delete, with
// the bloat, long vacuums and housekeeping competing with check-ins that come with it.
// Partitioned by month, retention drops whole months (instant, no bloat) and thinning
// rewrites an old month once into a thinned copy that replaces it.
//
// Months, not days as the plan first said: live keeps check-in history forever
// (checkin_retention_days 0) and thins it after 60 days, so daily partitions would grow
// by one a day without end, and every per-device read with no time range would visit
// all of them. Months keep it to about twelve a year; retention is a month coarser,
// which nobody reads that closely.
//
// Writers do not change: the table names stay. Rows whose time falls outside every
// month partition (a device clock in 1970 or 2099) land in <table>_pdefault.
//
// Turned on per server with PARTITION_TELEMETRY=1; until then nothing here runs and
// housekeeping keeps its DELETE paths. MigrateToPartitioned converts a table in the
// background: copy history month by month into a partitioned twin, then swap names in
// one short transaction. The old table is kept as <table>_unpart until DropUnpartitioned.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"
)

// PartSpec is one partitioned table: its time column and how far ahead to make months.
type PartSpec struct {
	Table string
	Col   string
}

// PartSpecs are the telemetry tables split by month.
var PartSpecs = []PartSpec{
	{Table: "device_samples", Col: "at"},
	{Table: "battery_samples", Col: "t"},
	{Table: "app_usage", Col: "bucket"},
}

// partMonthsAhead is how many months past the current one always exist, so a write
// never falls into the default partition just because the month turned.
const partMonthsAhead = 2

var partNameRE = regexp.MustCompile(`_p(\d{6})$`)

func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func partName(table string, month time.Time) string {
	return fmt.Sprintf("%s_p%s", table, month.Format("200601"))
}

// IsPartitioned reports whether table is a partitioned table.
func (d *DB) IsPartitioned(ctx context.Context, table string) (bool, error) {
	var ok bool
	err := d.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_partitioned_table pt JOIN pg_class c ON c.oid = pt.partrelid
		               WHERE c.relname = $1 AND c.relnamespace = 'public'::regnamespace)`, table).Scan(&ok)
	return ok, err
}

// PartitionMonths lists table's month partitions (start of each month), oldest first.
func (d *DB) PartitionMonths(ctx context.Context, table string) ([]time.Time, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT c.relname FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		JOIN pg_class p ON p.oid = i.inhparent
		WHERE p.relname = $1 AND p.relnamespace = 'public'::regnamespace
		ORDER BY c.relname`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		m := partNameRE.FindStringSubmatch(name)
		if m == nil {
			continue // the default partition
		}
		if t, err := time.Parse("200601", m[1]); err == nil {
			out = append(out, t)
		}
	}
	return out, rows.Err()
}

// ensureMonth adds month's partition to the partitioned table if it is missing. Rows of
// that month already sitting in the default partition move into it first, or the attach
// would fail its check against the default.
func (d *DB) ensureMonth(ctx context.Context, s PartSpec, month time.Time) error {
	name := partName(s.Table, month)
	var exists bool
	if err := d.pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+name).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	from, to := month, month.AddDate(0, 1, 0)
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE %s (LIKE %s INCLUDING DEFAULTS INCLUDING CONSTRAINTS)`, name, s.Table),
		fmt.Sprintf(`WITH moved AS (DELETE FROM %s_pdefault WHERE %s >= '%s' AND %s < '%s' RETURNING *)
			INSERT INTO %s SELECT * FROM moved`, s.Table, s.Col, from.Format(time.RFC3339), s.Col, to.Format(time.RFC3339), name),
		fmt.Sprintf(`ALTER TABLE %s ATTACH PARTITION %s FOR VALUES FROM ('%s') TO ('%s')`,
			s.Table, name, from.Format(time.RFC3339), to.Format(time.RFC3339)),
	}
	for _, q := range stmts {
		if _, err := tx.Exec(ctx, q); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return tx.Commit(ctx)
}

// EnsurePartitions makes the current month and partMonthsAhead after it exist for every
// partitioned telemetry table. Cheap when they do; run at start and from housekeeping.
func (d *DB) EnsurePartitions(ctx context.Context) error {
	now := monthStart(time.Now())
	for _, s := range PartSpecs {
		ok, err := d.IsPartitioned(ctx, s.Table)
		if err != nil || !ok {
			if err != nil {
				return err
			}
			continue
		}
		for i := 0; i <= partMonthsAhead; i++ {
			if err := d.ensureMonth(ctx, s, now.AddDate(0, i, 0)); err != nil {
				return err
			}
		}
	}
	return nil
}

// DropMonthsBefore removes table's whole months that end on or before cut, and deletes
// rows older than cut from the default partition. The month holding cut is kept whole:
// retention is a month coarse. Returns the partitions dropped.
func (d *DB) DropMonthsBefore(ctx context.Context, s PartSpec, cut time.Time) (int, error) {
	months, err := d.PartitionMonths(ctx, s.Table)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range months {
		if m.AddDate(0, 1, 0).After(cut) {
			continue
		}
		name := partName(s.Table, m)
		if _, err := d.pool.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s DETACH PARTITION %s`, s.Table, name)); err != nil {
			return n, fmt.Errorf("detach %s: %w", name, err)
		}
		if _, err := d.pool.Exec(ctx, fmt.Sprintf(`DROP TABLE %s`, name)); err != nil {
			return n, fmt.Errorf("drop %s: %w", name, err)
		}
		n++
	}
	if _, err := d.pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s_pdefault WHERE %s < $1`, s.Table, s.Col), cut); err != nil {
		return n, err
	}
	return n, nil
}

// ThinSamplesMonth replaces one whole month of device_samples older than olderThan with
// a copy keeping the first sample per device per bucketSec, the same rule
// DownsampleSamples applies row by row. A month is thinned once (its comment records
// it). Returns rows before and after, and whether a month was done.
func (d *DB) ThinSamplesMonth(ctx context.Context, olderThan time.Time, bucketSec int) (before, after int64, done bool, err error) {
	months, err := d.PartitionMonths(ctx, "device_samples")
	if err != nil {
		return 0, 0, false, err
	}
	for _, m := range months {
		if m.AddDate(0, 1, 0).After(olderThan) {
			break // months are oldest first; the rest are too recent
		}
		name := partName("device_samples", m)
		var note string
		_ = d.pool.QueryRow(ctx, `SELECT COALESCE(obj_description(to_regclass($1), 'pg_class'), '')`, "public."+name).Scan(&note)
		if strings.HasPrefix(note, "thinned") {
			continue
		}
		var empty bool
		if err := d.pool.QueryRow(ctx, fmt.Sprintf(`SELECT NOT EXISTS (SELECT 1 FROM %s)`, name)).Scan(&empty); err != nil {
			return 0, 0, false, err
		}
		if empty { // nothing to thin: mark it and look at the next month
			if _, err := d.pool.Exec(ctx, fmt.Sprintf(`COMMENT ON TABLE %s IS 'thinned (empty)'`, name)); err != nil {
				return 0, 0, false, err
			}
			continue
		}
		before, after, err = d.thinMonth(ctx, m, bucketSec)
		return before, after, err == nil, err
	}
	return 0, 0, false, nil
}

func (d *DB) thinMonth(ctx context.Context, m time.Time, bucketSec int) (int64, int64, error) {
	name := partName("device_samples", m)
	tmp := name + "_thin"
	from, to := m.Format(time.RFC3339), m.AddDate(0, 1, 0).Format(time.RFC3339)
	// Build the thinned copy outside the lock: an old month gets no writes but the odd
	// late backfill, which the locked step below picks up.
	if _, err := d.pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, tmp)); err != nil {
		return 0, 0, err
	}
	if _, err := d.pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (LIKE device_samples INCLUDING DEFAULTS INCLUDING CONSTRAINTS)`, tmp)); err != nil {
		return 0, 0, err
	}
	thin := fmt.Sprintf(`
		INSERT INTO %s
		SELECT DISTINCT ON (device_id, floor(EXTRACT(EPOCH FROM at) / %d)) *
		FROM %s %%s
		ORDER BY device_id, floor(EXTRACT(EPOCH FROM at) / %d), at
		ON CONFLICT DO NOTHING`, tmp, bucketSec, name, bucketSec)
	var cutAt time.Time
	if err := d.pool.QueryRow(ctx, fmt.Sprintf(`SELECT MAX(at) FROM %s`, name)).Scan(&cutAt); err != nil {
		return 0, 0, err
	}
	if _, err := d.pool.Exec(ctx, fmt.Sprintf(thin, "")); err != nil {
		return 0, 0, err
	}
	var before int64
	if err := d.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s`, name)).Scan(&before); err != nil {
		return 0, 0, err
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)
	// The swap: anything written since the copy started (rare: offline backfill) is
	// thinned in too, the old month leaves, the copy takes its place under its name.
	stmts := []string{
		`LOCK TABLE device_samples IN SHARE ROW EXCLUSIVE MODE`,
		fmt.Sprintf(thin, fmt.Sprintf("WHERE at > '%s'", cutAt.UTC().Format(time.RFC3339Nano))),
		fmt.Sprintf(`ALTER TABLE device_samples DETACH PARTITION %s`, name),
		fmt.Sprintf(`DROP TABLE %s`, name),
		fmt.Sprintf(`ALTER TABLE %s RENAME TO %s`, tmp, name),
		fmt.Sprintf(`ALTER TABLE device_samples ATTACH PARTITION %s FOR VALUES FROM ('%s') TO ('%s')`, name, from, to),
		fmt.Sprintf(`COMMENT ON TABLE %s IS 'thinned to %ds'`, name, bucketSec),
	}
	for _, q := range stmts {
		if _, err := tx.Exec(ctx, q); err != nil {
			return 0, 0, fmt.Errorf("thin %s: %w", name, err)
		}
	}
	var after int64
	if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s`, name)).Scan(&after); err != nil {
		return 0, 0, err
	}
	return before, after, tx.Commit(ctx)
}

// ── Converting a plain table ─────────────────────────────────────────────────

// partMigrateLock keeps two copies (or two housekeeping runs) from converting at once.
const partMigrateLock = 8_270_301

// MigrateToPartitioned converts s.Table into a partitioned table, if it is not one yet.
// Steps: a partitioned twin <table>_part with the same columns, keys, foreign keys and
// indexes; a partition per month from the oldest row to partMonthsAhead ahead, plus a
// default; history copied month by month (outside any lock, so writers go on); then one
// transaction that locks the old table against writes, copies what changed in the last
// two months again, and swaps the names. The old table stays as <table>_unpart.
func (d *DB) MigrateToPartitioned(ctx context.Context, s PartSpec) (bool, error) {
	done, err := d.IsPartitioned(ctx, s.Table)
	if err != nil || done {
		return false, err
	}
	conn, err := d.pool.Acquire(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Release()
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, partMigrateLock).Scan(&got); err != nil || !got {
		return false, err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, partMigrateLock)

	twin := s.Table + "_part"
	start := time.Now()
	if _, err := d.pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s CASCADE`, twin)); err != nil {
		return false, err
	}
	if _, err := d.pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (LIKE %s INCLUDING DEFAULTS) PARTITION BY RANGE (%s)`, twin, s.Table, s.Col)); err != nil {
		return false, err
	}
	// Keys and indexes as the original has them, renamed for the twin.
	defs, err := d.tableDefs(ctx, s.Table)
	if err != nil {
		return false, err
	}
	for _, q := range defs.forTable(twin) {
		if _, err := d.pool.Exec(ctx, q); err != nil {
			return false, fmt.Errorf("%s: %s: %w", twin, q, err)
		}
	}
	if _, err := d.pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s_pdefault PARTITION OF %s DEFAULT`, twin, twin)); err != nil {
		return false, err
	}
	var oldest *time.Time
	if err := d.pool.QueryRow(ctx, fmt.Sprintf(`SELECT MIN(%s) FROM %s WHERE %s > '2020-01-01'`, s.Col, s.Table, s.Col)).Scan(&oldest); err != nil {
		return false, err
	}
	first := monthStart(time.Now())
	if oldest != nil {
		first = monthStart(*oldest)
	}
	last := monthStart(time.Now()).AddDate(0, partMonthsAhead, 0)
	twinSpec := PartSpec{Table: twin, Col: s.Col}
	for m := first; !m.After(last); m = m.AddDate(0, 1, 0) {
		if err := d.ensureMonthNamed(ctx, twinSpec, m, partName(s.Table, m)); err != nil {
			return false, err
		}
	}
	// History, a month at a time, while writers carry on into the old table.
	for m := first; !m.After(last); m = m.AddDate(0, 1, 0) {
		if _, err := d.pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s SELECT * FROM %s WHERE %s >= $1 AND %s < $2 ON CONFLICT DO NOTHING`,
			twin, s.Table, s.Col, s.Col), m, m.AddDate(0, 1, 0)); err != nil {
			return false, err
		}
	}
	if _, err := d.pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s SELECT * FROM %s WHERE %s < $1 OR %s >= $2 ON CONFLICT DO NOTHING`,
		twin, s.Table, s.Col, s.Col), first, last.AddDate(0, 1, 0)); err != nil {
		return false, err
	}

	// The swap. Writers wait on the lock for the length of a two-month re-copy.
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	// Only what can have changed since the copy began needs copying again under the lock:
	// rows written since, and the Standard client's offline backfill, which carries device
	// timestamps up to 7 days old. Two whole months (the first cut) held live's writers
	// for the length of most of the table.
	since := start.Add(-8 * 24 * time.Hour)
	stmts := []string{
		`SET LOCAL lock_timeout = '30s'`,
		fmt.Sprintf(`LOCK TABLE %s IN SHARE ROW EXCLUSIVE MODE`, s.Table),
		// Recent rows may have been written (or, for app_usage, added to) since the copy.
		fmt.Sprintf(`DELETE FROM %s WHERE %s >= '%s'`, twin, s.Col, since.Format(time.RFC3339)),
		fmt.Sprintf(`INSERT INTO %s SELECT * FROM %s WHERE %s >= '%s' ON CONFLICT DO NOTHING`, twin, s.Table, s.Col, since.Format(time.RFC3339)),
	}
	if s.Table == "app_usage" {
		// app_usage rows are added to after they are written (hourly sums, the daily
		// rollup), at any age: copy the whole small table again inside the lock.
		stmts = append(stmts[:2], fmt.Sprintf(`TRUNCATE %s`, twin),
			fmt.Sprintf(`INSERT INTO %s SELECT * FROM %s`, twin, s.Table))
	}
	stmts = append(stmts, defs.renameOld(s.Table)...)
	stmts = append(stmts,
		fmt.Sprintf(`ALTER TABLE %s RENAME TO %s_unpart`, s.Table, s.Table),
		fmt.Sprintf(`ALTER TABLE %s RENAME TO %s`, twin, s.Table),
		fmt.Sprintf(`ALTER TABLE %s_pdefault RENAME TO %s_pdefault`, twin, s.Table),
		fmt.Sprintf(`COMMENT ON TABLE %s_unpart IS 'unpartitioned at %s'`, s.Table, time.Now().UTC().Format(time.RFC3339)),
	)
	stmts = append(stmts, defs.renameTwin(twin, s.Table)...)
	for _, q := range stmts {
		if _, err := tx.Exec(ctx, q); err != nil {
			return false, fmt.Errorf("swap %s: %s: %w", s.Table, q, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	log.Printf("[partitions] %s is partitioned by month (%s); the old table is %s_unpart", s.Table, time.Since(start).Round(time.Second), s.Table)
	return true, nil
}

// ensureMonthNamed is ensureMonth for the twin, whose partitions already carry the final
// table's names (device_samples_p202610 under device_samples_part), so nothing needs
// renaming at the swap.
func (d *DB) ensureMonthNamed(ctx context.Context, s PartSpec, month time.Time, name string) error {
	from, to := month, month.AddDate(0, 1, 0)
	_, err := d.pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`,
		name, s.Table, from.Format(time.RFC3339), to.Format(time.RFC3339)))
	return err
}

// tableConstraint and tableIndex are a table's keys and secondary indexes, as SQL.
type tableDefs struct {
	constraints []struct{ name, def string }
	indexes     []struct{ name, def string }
}

func (d *DB) tableDefs(ctx context.Context, table string) (tableDefs, error) {
	var t tableDefs
	rows, err := d.pool.Query(ctx, `
		SELECT conname, pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid = to_regclass($1) AND contype IN ('p', 'f', 'u', 'c') ORDER BY contype DESC, conname`, "public."+table)
	if err != nil {
		return t, err
	}
	for rows.Next() {
		var c struct{ name, def string }
		if err := rows.Scan(&c.name, &c.def); err != nil {
			rows.Close()
			return t, err
		}
		t.constraints = append(t.constraints, c)
	}
	rows.Close()
	rows, err = d.pool.Query(ctx, `
		SELECT i.relname, pg_get_indexdef(i.oid) FROM pg_index x JOIN pg_class i ON i.oid = x.indexrelid
		WHERE x.indrelid = to_regclass($1) AND NOT x.indisprimary
		  AND NOT EXISTS (SELECT 1 FROM pg_constraint c WHERE c.conindid = x.indexrelid)`, "public."+table)
	if err != nil {
		return t, err
	}
	defer rows.Close()
	for rows.Next() {
		var c struct{ name, def string }
		if err := rows.Scan(&c.name, &c.def); err != nil {
			return t, err
		}
		t.indexes = append(t.indexes, c)
	}
	return t, rows.Err()
}

// forTable is the DDL that gives twin the same constraints and indexes, under "_part"
// names (renamed to the originals' at the swap).
func (t tableDefs) forTable(twin string) []string {
	var out []string
	for _, c := range t.constraints {
		out = append(out, fmt.Sprintf(`ALTER TABLE %s ADD CONSTRAINT %s_part %s`, twin, c.name, c.def))
	}
	for _, ix := range t.indexes {
		def := ix.def
		// "CREATE INDEX idx ON public.tbl USING btree (...)" → on the twin, new name.
		if i := strings.Index(def, " ON "); i >= 0 {
			rest := def[i+4:]
			if j := strings.Index(rest, " "); j >= 0 {
				rest = rest[j:]
			}
			def = fmt.Sprintf("CREATE INDEX %s_part ON %s%s", ix.name, twin, rest)
		}
		out = append(out, def)
	}
	return out
}

// renameOld frees the original names: the old table's constraints and indexes get "_unpart".
func (t tableDefs) renameOld(table string) []string {
	var out []string
	for _, c := range t.constraints {
		out = append(out, fmt.Sprintf(`ALTER TABLE %s RENAME CONSTRAINT %s TO %s_unpart`, table, c.name, c.name))
	}
	for _, ix := range t.indexes {
		out = append(out, fmt.Sprintf(`ALTER INDEX %s RENAME TO %s_unpart`, ix.name, ix.name))
	}
	return out
}

// renameTwin gives the twin's constraints and indexes the original names.
func (t tableDefs) renameTwin(twin, table string) []string {
	var out []string
	for _, c := range t.constraints {
		out = append(out, fmt.Sprintf(`ALTER TABLE %s RENAME CONSTRAINT %s_part TO %s`, table, c.name, c.name))
	}
	for _, ix := range t.indexes {
		out = append(out, fmt.Sprintf(`ALTER INDEX %s_part RENAME TO %s`, ix.name, ix.name))
	}
	return out
}

// DropUnpartitioned removes the old plain tables a migration kept, once they are older
// than keep (counted from the swap, recorded in the table comment).
func (d *DB) DropUnpartitioned(ctx context.Context, keep time.Duration) error {
	var errs []error
	for _, s := range PartSpecs {
		old := s.Table + "_unpart"
		var exists bool
		if err := d.pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+old).Scan(&exists); err != nil || !exists {
			continue
		}
		var note string
		_ = d.pool.QueryRow(ctx, `SELECT COALESCE(obj_description(to_regclass($1), 'pg_class'), '')`, "public."+old).Scan(&note)
		at, err := time.Parse(time.RFC3339, strings.TrimPrefix(note, "unpartitioned at "))
		if err != nil {
			_, err = d.pool.Exec(ctx, fmt.Sprintf(`COMMENT ON TABLE %s IS 'unpartitioned at %s'`, old, time.Now().UTC().Format(time.RFC3339)))
			if err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if time.Since(at) < keep {
			continue
		}
		if _, err := d.pool.Exec(ctx, fmt.Sprintf(`DROP TABLE %s`, old)); err != nil {
			errs = append(errs, err)
			continue
		}
		log.Printf("[partitions] dropped %s (kept %s after the swap)", old, keep)
	}
	return errors.Join(errs...)
}

// PruneBatteryPartitioned is PruneBatteryData for a partitioned battery_samples: the
// whole months already went (DropMonthsBefore); this trims the month the cut falls in and
// the charge climbs, with plain range deletes.
func (d *DB) PruneBatteryPartitioned(ctx context.Context, keep time.Duration) (int64, error) {
	cut := time.Now().Add(-keep)
	a, err := d.pool.Exec(ctx, `DELETE FROM battery_samples WHERE t < $1`, cut)
	if err != nil {
		return 0, err
	}
	b, err := d.pool.Exec(ctx, `DELETE FROM battery_climbs WHERE at < $1`, cut)
	if err != nil {
		return a.RowsAffected(), err
	}
	return a.RowsAffected() + b.RowsAffected(), nil
}
