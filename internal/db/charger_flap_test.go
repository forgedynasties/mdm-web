package db

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func flapReport(charging bool, typ string, extra string) json.RawMessage {
	b, _ := json.Marshal(charging)
	s := `{"charging":` + string(b) + `,"charger_type":"` + typ + `","uptime_seconds":1`
	if extra != "" {
		s += "," + extra
	}
	return json.RawMessage(s + "}")
}

func chargingOf(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return string(m["charging"])
}

// toggle drives n toggles one second apart through the tracker, returning the last pair.
func toggle(tr *flapTracker, id uuid.UUID, start time.Time, n int, state bool) (json.RawMessage, json.RawMessage, time.Time, bool) {
	var hp, hc json.RawMessage
	now := start
	for i := 0; i < n; i++ {
		prev := flapReport(state, typeFor(state), "")
		state = !state
		hp, hc = tr.project(id, prev, flapReport(state, typeFor(state), ""), now)
		now = now.Add(time.Second)
	}
	return hp, hc, now, state
}

func typeFor(charging bool) string {
	if charging {
		return "usb"
	}
	return "none"
}

func TestSteadyChargerIsUntouched(t *testing.T) {
	var tr flapTracker
	id := uuid.New()
	prev, cur := flapReport(true, "usb", ""), flapReport(true, "usb", "")
	hp, hc := tr.project(id, prev, cur, time.Now())
	if string(hp) != string(prev) || string(hc) != string(cur) {
		t.Fatal("a device that is not flapping must pass through unchanged")
	}
	if len(tr.m) != 0 {
		t.Fatal("nothing should be remembered for a steady device")
	}
}

func TestAPlugAndUnplugIsNotAFlap(t *testing.T) {
	var tr flapTracker
	_, hc, _, _ := toggle(&tr, uuid.New(), time.Now(), flapEnterFlips-1, true)
	if got := chargingOf(t, hc); got == flapCharging {
		t.Fatalf("%d toggles must not be a flap, got charging=%s", flapEnterFlips-1, got)
	}
}

func TestFlappingIsOneStateThenSettles(t *testing.T) {
	var tr flapTracker
	id := uuid.New()
	start := time.Now()

	// Enough toggles to start a flap: the pair that crosses the threshold goes real → 2.
	hp, hc, now, state := toggle(&tr, id, start, flapEnterFlips, true)
	if chargingOf(t, hp) == flapCharging || chargingOf(t, hc) != flapCharging {
		t.Fatalf("entering: want real → 2, got %s → %s", chargingOf(t, hp), chargingOf(t, hc))
	}

	// Toggles inside the flap: 2 → 2, and charger_type held, so the hash does not move.
	hp, hc, now, state = toggle(&tr, id, now, 40, state)
	if string(hp) != string(hc) {
		t.Fatalf("inside the flap history must not change:\n%s\n%s", hp, hc)
	}
	if r := tr.rates(now)[id]; r < flapEnterFlips {
		t.Fatalf("flapping device should report its rate, got %d", r)
	}

	// Quiet, but not for long enough: still one state.
	quiet := now.Add(flapSettle / 2)
	_, hc = tr.project(id, flapReport(state, typeFor(state), ""), flapReport(state, typeFor(state), ""), quiet)
	if chargingOf(t, hc) != flapCharging {
		t.Fatal("a flap must not end before flapSettle")
	}

	// Quiet for flapSettle: 2 → the settled value, which earns one row and one event.
	done := now.Add(flapSettle + time.Second)
	hp, hc = tr.project(id, flapReport(state, typeFor(state), ""), flapReport(state, typeFor(state), ""), done)
	if chargingOf(t, hp) != flapCharging || chargingOf(t, hc) == flapCharging {
		t.Fatalf("settling: want 2 → real, got %s → %s", chargingOf(t, hp), chargingOf(t, hc))
	}
	if _, ok := tr.rates(done)[id]; ok {
		t.Fatal("a settled device must not be reported as flapping")
	}
}

func TestClientReportedFlap(t *testing.T) {
	var tr flapTracker
	id := uuid.New()
	now := time.Now()
	calm := flapReport(true, "usb", `"charger_flapping":false,"charger_flaps_5m":0`)
	flap := flapReport(true, "usb", `"charger_flapping":true,"charger_flaps_5m":250`)

	hp, hc := tr.project(id, calm, flap, now)
	if chargingOf(t, hp) == flapCharging || chargingOf(t, hc) != flapCharging {
		t.Fatalf("client flag: want real → 2, got %s → %s", chargingOf(t, hp), chargingOf(t, hc))
	}
	if r := tr.rates(now)[id]; r != 50 {
		t.Fatalf("rate should come from charger_flaps_5m/5, got %d", r)
	}
	hp, hc = tr.project(id, flap, calm, now.Add(time.Minute))
	if chargingOf(t, hp) != flapCharging || chargingOf(t, hc) == flapCharging {
		t.Fatalf("client settle: want 2 → real, got %s → %s", chargingOf(t, hp), chargingOf(t, hc))
	}
}

func TestSilentFlapperIsForgotten(t *testing.T) {
	var tr flapTracker
	id := uuid.New()
	_, _, now, _ := toggle(&tr, id, time.Now(), flapEnterFlips, true)
	if _, ok := tr.rates(now)[id]; !ok {
		t.Fatal("should be flapping")
	}
	if _, ok := tr.rates(now.Add(flapForget + time.Minute))[id]; ok {
		t.Fatal("a device silent past flapForget must not still be reported as flapping")
	}
}
