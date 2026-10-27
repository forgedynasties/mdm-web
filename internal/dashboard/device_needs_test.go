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
