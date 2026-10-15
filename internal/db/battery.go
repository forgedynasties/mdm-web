package db

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// BatterySample is one row of a device's battery log (client 1.10.1, POST
// /api/v1/battery-log). Pointers are absent or "unknown" readings (the client's -1 / -100).
type BatterySample struct {
	T      time.Time // the device's clock
	UpS    int64
	Ev     string // "b" battery broadcast, "s" screen edge
	MV     *int32
	Pct    *int16
	Status *int16
	Plug   *int16
	TempC  *float32
	Screen *bool
	IChgMA *int32
	IInMA  *int32
	Flap   *bool
	Wlc    *int16
	WlcOn  *int16
}

// InsertBatterySamples stores a batch in one statement. A row already stored (the same
// device, t, up_s and ev) is skipped, so a batch resent after a lost acknowledgement is
// harmless. Returns how many rows were new.
func (d *DB) InsertBatterySamples(ctx context.Context, deviceID uuid.UUID, fixMs int64, rows []BatterySample) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	n := len(rows)
	t := make([]time.Time, n)
	up := make([]int64, n)
	ev := make([]string, n)
	mv := make([]*int32, n)
	pct := make([]*int16, n)
	st := make([]*int16, n)
	plug := make([]*int16, n)
	temp := make([]*float32, n)
	scr := make([]*bool, n)
	ichg := make([]*int32, n)
	iin := make([]*int32, n)
	flap := make([]*bool, n)
	wlc := make([]*int16, n)
	wlcOn := make([]*int16, n)
	for i, r := range rows {
		t[i], up[i], ev[i], mv[i], pct[i], st[i], plug[i] = r.T, r.UpS, r.Ev, r.MV, r.Pct, r.Status, r.Plug
		temp[i], scr[i], ichg[i], iin[i], flap[i], wlc[i], wlcOn[i] = r.TempC, r.Screen, r.IChgMA, r.IInMA, r.Flap, r.Wlc, r.WlcOn
	}
	tag, err := d.pool.Exec(ctx, `
		INSERT INTO battery_samples (device_id, t, up_s, ev, mv, pct, status, plug, temp_c, screen, ichg_ma, iin_ma, flap, wlc, wlc_on, t_fix_ms)
		SELECT $1, u.t, u.up_s, u.ev, u.mv, u.pct, u.status, u.plug, u.temp_c, u.screen, u.ichg_ma, u.iin_ma, u.flap, u.wlc, u.wlc_on, $16
		FROM unnest($2::timestamptz[], $3::bigint[], $4::text[], $5::int[], $6::smallint[], $7::smallint[], $8::smallint[],
		            $9::real[], $10::boolean[], $11::int[], $12::int[], $13::boolean[], $14::smallint[], $15::smallint[])
		     AS u(t, up_s, ev, mv, pct, status, plug, temp_c, screen, ichg_ma, iin_ma, flap, wlc, wlc_on)
		ON CONFLICT DO NOTHING`,
		deviceID, t, up, ev, mv, pct, st, plug, temp, scr, ichg, iin, flap, wlc, wlcOn, fixMs)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// insertBatteryClimb keeps extra.battery_cc as a row of battery_climbs when the report
// carries one. A report without it, or with one that can't be read, is left alone.
func insertBatteryClimb(ctx context.Context, tx pgx.Tx, deviceID uuid.UUID, extra json.RawMessage) error {
	if len(extra) == 0 {
		return nil
	}
	var probe struct {
		CC json.RawMessage `json:"battery_cc"`
	}
	if json.Unmarshal(extra, &probe) != nil || len(probe.CC) == 0 || string(probe.CC) == "null" {
		return nil
	}
	var cc struct {
		At int64 `json:"at"`
	}
	if json.Unmarshal(probe.CC, &cc) != nil || cc.At <= 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO battery_climbs (device_id, at, payload) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		deviceID, time.UnixMilli(cc.At).UTC(), probe.CC)
	return err
}

// PruneBatteryData drops battery samples and climbs older than keep, in slices so a large
// backlog never holds a long lock.
func (d *DB) PruneBatteryData(ctx context.Context, keep time.Duration) (int64, error) {
	cut := time.Now().Add(-keep)
	var total int64
	for {
		tag, err := d.pool.Exec(ctx, `
			DELETE FROM battery_samples WHERE ctid IN (SELECT ctid FROM battery_samples WHERE t < $1 LIMIT 20000)`, cut)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < 20000 {
			break
		}
	}
	tag, err := d.pool.Exec(ctx, `DELETE FROM battery_climbs WHERE at < $1`, cut)
	if err == nil {
		total += tag.RowsAffected()
	}
	return total, err
}
