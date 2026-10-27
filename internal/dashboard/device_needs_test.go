package dashboard

import (
	"testing"

	"mdm/internal/db"
)

// N1 (27 Oct 2026): worst first, a reason each, a button only where something can act.
func TestDeviceNeedsOf(t *testing.T) {
	sec := deviceSecurity{AdbOn: true, UnknownSources: "On", UnknownApps: []string{"aio.app.nugget"}, USBDebugging: "On", DevOptions: "On"}
	got := deviceNeedsOf(sec, &db.Device{BatteryPct: 12}, true, true)
	want := []string{"Wireless adb open", "Installs from unknown sources allowed", "USB debugging on", "Developer options on", "Battery 12%", "Charger flapping"}
	if len(got) != len(want) {
		t.Fatalf("got %d needs: %+v", len(got), got)
	}
	for i, w := range want {
		if got[i].Title != w || got[i].Why == "" {
			t.Errorf("need %d = %+v, want %q", i, got[i], w)
		}
	}
	if got[0].Tone != "bad" || got[0].Action != "adb" || got[1].Why != "aio.app.nugget may install apps outside the MDM" || got[4].Action != "" {
		t.Errorf("details: %+v", got)
	}
	if n := deviceNeedsOf(deviceSecurity{USBDebugging: "Off", DevOptions: "—"}, &db.Device{BatteryPct: 80}, true, false); len(n) != 0 {
		t.Errorf("a clean device has needs: %+v", n)
	}
}

// Overview and Health share one list: alerts and failing checks join the posture rows,
// and a check a posture row already says is not listed twice (27 Oct 2026).
func TestDeviceTodoOf(t *testing.T) {
	needs := deviceNeedsOf(deviceSecurity{AdbOn: true, USBDebugging: "On"}, &db.Device{BatteryPct: 10}, true, false)
	health := deviceHealth{Items: []healthItem{
		{Kind: "alert", Title: "Offline during service", Tone: "danger"},
		{Kind: "alert", Title: "Running hot", Tone: "warn", Note: "52 °C"},
		{Kind: "compliance", CheckID: "adb_off", Title: "USB debugging on"},
		{Kind: "compliance", CheckID: "checked_in", Title: "Not checked in recently", Note: "last seen 2 days ago"},
	}}
	got := deviceTodoOf(needs, health)
	want := []string{"Offline during service", "Wireless adb open", "Running hot", "USB debugging on", "Not checked in recently", "Battery 10%"}
	if len(got) != len(want) {
		t.Fatalf("todo = %+v", got)
	}
	for i, w := range want {
		if got[i].Title != w {
			t.Errorf("row %d = %q, want %q", i, got[i].Title, w)
		}
	}
	if got[0].Tone != "bad" || got[2].Why != "52 °C" || got[4].Action != "health" {
		t.Errorf("details: %+v", got)
	}
}
