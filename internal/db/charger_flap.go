package db

import (
	"bytes"
	"encoding/json"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Charger flapping, as history stores it.
//
// A charger with a bad cable toggles `charging` every second or two. Each toggle is a
// state change, and state changes are stored the instant they happen — they skip the
// sample window on purpose — so one faulty T7 (AT070AABU00509, 24 Sep 2026) wrote 2,440
// history rows and 2,424 events in an hour: more than half of the fleet's rows.
//
// While a device flaps, its history records charging as "2" instead of true/false, and
// charger_type is held at the value it had going in. The toggles inside the flap leave
// that projection unchanged, so they coalesce like any volatile reading; a row is earned
// when the flap starts (true/false → 2) and when it ends (2 → the settled value).
//
// Only history sees the "2": the state hash and device_state_events. devices.latest_extra
// keeps what the device reported, because a dozen readers cast it to boolean and the live
// view should show the live value.
//
// A flap is recognised two ways: the server counting toggles itself (any client), or a
// client that debounces on the device (firmware 1.4.5+) saying charger_flapping=true — it
// stops sending the toggles, so there is nothing left here to count. Same thresholds on
// both sides (ChargerFlap.java).
const (
	flapEnterFlips = 6               // toggles inside flapWindow that start a flap
	flapWindow     = time.Minute     // the span flapEnterFlips is counted over
	flapSettle     = 2 * time.Minute // this long without a toggle and the flap is over
	flapCharging   = `2`             // what history stores for charging during a flap

	// flapForget drops a device that stopped reporting mid-flap, so the alert does not
	// keep calling an offline device's charger faulty. Longer than the firmware client's
	// 5-minute keyframe, the longest a debounced, flapping device goes without a report.
	flapForget = 10 * time.Minute
)

type flapState struct {
	flips      []time.Time     // toggles seen here, the last flapWindow of them
	lastFlip   time.Time       // zero for a flap only the client has seen
	flapping   bool            // inside a flap now
	seen       time.Time       // the last report, for flapForget
	heldType   json.RawMessage // charger_type as the flap began; nil when there was none
	clientRate int             // toggles/min the client reports (charger_flaps_5m / 5)
}

// flapTracker is per process: this server is the only writer, and after a restart a
// device still flapping is recognised again within its next few toggles.
type flapTracker struct {
	mu sync.Mutex
	m  map[uuid.UUID]*flapState
}

// flapKeys is the little of a report the tracker reads, decoded without building a map.
type flapKeys struct {
	Charging        json.RawMessage `json:"charging"`
	ChargerType     json.RawMessage `json:"charger_type"`
	ChargerFlapping *bool           `json:"charger_flapping"`
	ChargerFlaps5m  *int            `json:"charger_flaps_5m"`
}

// project returns the report pair history should compare: prev and cur unchanged for a
// device that is not flapping (the common case, and no allocation), or with charging set
// to "2" and charger_type held on whichever side of the pair was inside a flap.
func (t *flapTracker) project(id uuid.UUID, prev, cur json.RawMessage, now time.Time) (json.RawMessage, json.RawMessage) {
	var p, c flapKeys
	_ = json.Unmarshal(prev, &p)
	if err := json.Unmarshal(cur, &c); err != nil {
		return prev, cur
	}
	clientSays := c.ChargerFlapping != nil && *c.ChargerFlapping
	toggled := len(p.Charging) > 0 && len(c.Charging) > 0 && !bytes.Equal(p.Charging, c.Charging)

	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.m[id]
	if st == nil {
		if !toggled && !clientSays {
			return prev, cur
		}
		if t.m == nil {
			t.m = make(map[uuid.UUID]*flapState)
		}
		st = &flapState{}
		t.m[id] = st
	}

	st.seen = now
	if toggled {
		st.lastFlip = now
		st.flips = append(st.flips, now)
	}
	keep := st.flips[:0]
	for _, f := range st.flips {
		if now.Sub(f) <= flapWindow {
			keep = append(keep, f)
		}
	}
	st.flips = keep
	if c.ChargerFlaps5m != nil {
		st.clientRate = (*c.ChargerFlaps5m + 4) / 5
	}

	was := st.flapping
	switch {
	case !was && (len(st.flips) >= flapEnterFlips || clientSays):
		st.flapping = true
		st.heldType = append(json.RawMessage(nil), p.ChargerType...)
		if len(st.heldType) == 0 {
			st.heldType = append(json.RawMessage(nil), c.ChargerType...)
		}
	case was && !clientSays && now.Sub(st.lastFlip) >= flapSettle:
		st.flapping = false
	}
	held := st.heldType
	if !st.flapping && len(st.flips) == 0 {
		delete(t.m, id) // nothing left to remember
	}

	if was {
		prev = withFlap(prev, held)
	}
	if st.flapping {
		cur = withFlap(cur, held)
	}
	return prev, cur
}

// withFlap rewrites a report as history sees it during a flap.
func withFlap(raw json.RawMessage, heldType json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return raw
	}
	m["charging"] = json.RawMessage(flapCharging)
	if len(heldType) > 0 {
		m["charger_type"] = heldType
	} else {
		delete(m, "charger_type")
	}
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// rates returns the devices flapping now, with their toggles per minute: the higher of
// what the server counted over the last minute and what the client reports.
func (t *flapTracker) rates(now time.Time) map[uuid.UUID]int {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[uuid.UUID]int)
	for id, st := range t.m {
		if now.Sub(st.seen) > flapForget {
			delete(t.m, id)
			continue
		}
		if !st.flapping {
			continue
		}
		r := len(st.flips) // flapWindow is one minute
		if st.clientRate > r {
			r = st.clientRate
		}
		out[id] = r
	}
	return out
}
