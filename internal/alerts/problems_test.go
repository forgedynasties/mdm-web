package alerts

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

func TestCauseOf(t *testing.T) {
	for _, c := range []struct {
		types []string
		want  string
	}{
		{[]string{"offline", "battery_low"}, "offline"},
		{[]string{"offline_peak", "wifi_unstable"}, "wifi"},
		{[]string{"slow_charge_night", "battery_low"}, "charging"},
		{[]string{"charger_flapping"}, "charging"},
		{[]string{"slow_charge_night"}, "slow_charge"},
		{[]string{"battery_low"}, "battery"},
		{[]string{"overheating", "memory_low"}, "heat"},
		{[]string{"device_crash"}, "crash"},
		{[]string{"something_new"}, "other"},
	} {
		if got := CauseOf(c.types).Key; got != c.want {
			t.Errorf("CauseOf(%v) = %s, want %s", c.types, got, c.want)
		}
	}
}

func TestBuildProblemMessages(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	n := func(dev uuid.UUID, typ, sev, serial string) db.AlertNotification {
		return db.AlertNotification{Type: typ, Severity: sev, Serial: serial, Summary: typ + " on " + serial, DeviceID: dev, AlertID: uuid.New()}
	}
	info := map[uuid.UUID]db.DeviceNotifyInfo{a: {Restaurant: "Flights Vegas"}, b: {Restaurant: "Flights Vegas"}, c: {Restaurant: "Pho 88"}}
	msgs := buildProblemMessages([]db.AlertNotification{
		n(a, "offline_peak", "critical", "T7-A"),
		n(b, "offline_peak", "critical", "T7-B"),
		n(c, "charger_flapping", "warning", "T7-C"),
		n(c, "battery_low", "critical", "T7-C"),
	}, info)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2: %+v", len(msgs), msgs)
	}
	if m := msgs[0]; !strings.Contains(m.Title, "Flights Vegas: 2 devices offline") || m.Severity != "critical" || len(m.IDs) != 2 {
		t.Errorf("restaurant message = %+v", m)
	}
	if m := msgs[1]; !strings.Contains(m.Title, "T7-C · Pho 88: Charging hardware") || m.Severity != "critical" ||
		len(m.IDs) != 2 || !strings.Contains(m.Text, "charger_flapping on T7-C") {
		t.Errorf("device message = %+v", m)
	}
}

func TestMorningDigest(t *testing.T) {
	day := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	a := uuid.New()
	title, text, lines := morningDigest(day, []db.NightAlert{
		{DeviceID: a, Serial: "T7-A", Restaurant: "Pho 88", Type: "slow_charge_night"},
		{DeviceID: a, Serial: "T7-A", Restaurant: "Pho 88", Type: "battery_low", Open: true},
	}, map[string]int{"Flights Vegas": 2})
	if lines != 2 || !strings.Contains(title, "Wed 30 Sep") || !strings.Contains(text, "T7-A (Pho 88): Charging hardware — still open") ||
		!strings.Contains(text, "2 placed device(s) not reporting — Flights Vegas 2") {
		t.Errorf("digest = %q / %q / %d", title, text, lines)
	}
	if _, _, lines := morningDigest(day, nil, nil); lines != 0 {
		t.Errorf("quiet night should send nothing, got %d lines", lines)
	}
}
