package db

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Offline check-ins (1 Oct; plan: static/offline-queue-plan.html). A device that can't
// reach the MDM keeps a reading every few minutes and sends them when it can. They go
// into the same history as live readings, marked late, and never raise alerts.

// LateSample is one reading a device kept while offline.
type LateSample struct {
	At            time.Time
	BatteryPct    *int16
	TempC         *float64
	WifiRSSI      *int16
	RAMUsedMB     *int32
	RAMTotalMB    *int32
	StorageFreeGB *float64
}

// InsertLateSamples stores late readings; a moment the history already has is kept as
// it was. Returns how many were new.
func (d *DB) InsertLateSamples(ctx context.Context, deviceID uuid.UUID, ss []LateSample) (int, error) {
	if len(ss) == 0 {
		return 0, nil
	}
	n := len(ss)
	dev := make([]uuid.UUID, n)
	at := make([]time.Time, n)
	bat := make([]*int16, n)
	temp := make([]*float64, n)
	rssi := make([]*int16, n)
	ramU := make([]*int32, n)
	ramT := make([]*int32, n)
	sto := make([]*float64, n)
	for i, s := range ss {
		dev[i], at[i], bat[i], temp[i], rssi[i], ramU[i], ramT[i], sto[i] = deviceID, s.At, s.BatteryPct, s.TempC, s.WifiRSSI, s.RAMUsedMB, s.RAMTotalMB, s.StorageFreeGB
	}
	// A late reading's time is worked out when the batch arrives, so a batch sent again
	// (its reply was lost) lands a few seconds off the first copy. Anything within 90 s
	// of a reading the history already has is that reading, live or late: readings kept
	// offline are minutes apart, so real ones never collide.
	tag, err := d.pool.Exec(ctx, `
		INSERT INTO device_samples (device_id, at, battery_pct, temp_c, wifi_rssi, ram_used_mb, ram_total_mb, storage_free_gb, late)
		SELECT u.d, u.a, u.b, u.t, u.r, u.ru, u.rt, u.s, true
		FROM unnest($1::uuid[], $2::timestamptz[], $3::int2[], $4::float8[], $5::int2[], $6::int4[], $7::int4[], $8::float8[])
		     AS u(d, a, b, t, r, ru, rt, s)
		WHERE NOT EXISTS (SELECT 1 FROM device_samples x
		                  WHERE x.device_id = u.d AND x.at BETWEEN u.a - interval '90 seconds' AND u.a + interval '90 seconds')
		ON CONFLICT (device_id, at) DO NOTHING`, dev, at, bat, temp, rssi, ramU, ramT, sto)
	return int(tag.RowsAffected()), err
}

// TimeSpan is a stretch of time.
type TimeSpan struct {
	From int64 `json:"from"` // unix ms
	To   int64 `json:"to"`
}

// LateRuns groups the times of late readings into spans: readings further apart than
// gap start a new span. Times must be in order.
func LateRuns(times []time.Time, gap time.Duration) []TimeSpan {
	var out []TimeSpan
	for _, t := range times {
		x := t.UnixMilli()
		if n := len(out); n > 0 && x-out[n-1].To <= gap.Milliseconds() {
			out[n-1].To = x
			continue
		}
		out = append(out, TimeSpan{From: x, To: x})
	}
	return out
}

// LateSpans returns the device's late-reading spans in a window, for the device page.
func (d *DB) LateSpans(ctx context.Context, deviceID uuid.UUID, from, until time.Time, gap time.Duration) ([]TimeSpan, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT at FROM device_samples WHERE device_id = $1 AND late AND at >= $2 AND at <= $3 ORDER BY at`, deviceID, from, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ts []time.Time
	for rows.Next() {
		var t time.Time
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		ts = append(ts, t)
	}
	return LateRuns(ts, gap), rows.Err()
}

// RecordDeviceEvent adds a line to the device's history.
func (d *DB) RecordDeviceEvent(ctx context.Context, deviceID uuid.UUID, kind, summary string) {
	_, _ = d.pool.Exec(ctx, `INSERT INTO device_events (device_id, kind, summary, occurred_at) VALUES ($1, $2, $3, NOW())`, deviceID, kind, summary)
}
