package db

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Host app on screen, for the testing team's API (21 Oct 2026). The firmware client
// (1.11.0+) reports extra.host_app; at ingest withDerivedState turns "is the variant in
// use in the foreground" into the host_app_fg state key, and screen_on is a report field
// of its own. Both are stored as transitions in device_state_events, so the on-screen
// timeline is a replay of those two keys.

// ScreenStateEvent is one transition of host_app_fg or screen_on.
type ScreenStateEvent struct {
	DeviceID uuid.UUID
	At       time.Time
	Key      string // host_app_fg | screen_on
	To       string // "true" | "false"
}

// HostAppScreenKeys are the state keys the timeline replays.
var HostAppScreenKeys = []string{"host_app_fg", "screen_on"}

// HostAppScreenEvents returns, for each device, the value each key held at `from` (its
// last transition before the window, "" when none is kept) and the transitions inside
// [from, to), oldest first.
func (d *DB) HostAppScreenEvents(ctx context.Context, ids []uuid.UUID, from, to time.Time) (map[uuid.UUID]map[string]string, map[uuid.UUID][]ScreenStateEvent, error) {
	initial := map[uuid.UUID]map[string]string{}
	events := map[uuid.UUID][]ScreenStateEvent{}
	if len(ids) == 0 {
		return initial, events, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT DISTINCT ON (device_id, key) device_id, key, to_val
		FROM device_state_events
		WHERE device_id = ANY($1) AND key = ANY($2) AND at < $3
		ORDER BY device_id, key, at DESC`, ids, HostAppScreenKeys, from)
	if err != nil {
		return nil, nil, fmt.Errorf("host app initial state: %w", err)
	}
	for rows.Next() {
		var id uuid.UUID
		var key, val string
		if err := rows.Scan(&id, &key, &val); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if initial[id] == nil {
			initial[id] = map[string]string{}
		}
		initial[id][key] = val
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	rows, err = d.pool.Query(ctx, `
		SELECT device_id, at, key, to_val
		FROM device_state_events
		WHERE device_id = ANY($1) AND key = ANY($2) AND at >= $3 AND at < $4
		ORDER BY device_id, at`, ids, HostAppScreenKeys, from, to)
	if err != nil {
		return nil, nil, fmt.Errorf("host app events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e ScreenStateEvent
		if err := rows.Scan(&e.DeviceID, &e.At, &e.Key, &e.To); err != nil {
			return nil, nil, err
		}
		events[e.DeviceID] = append(events[e.DeviceID], e)
	}
	return initial, events, rows.Err()
}
