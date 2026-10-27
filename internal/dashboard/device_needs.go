package dashboard

import (
	"fmt"
	"strings"

	"mdm/internal/db"
)

// Needs a look (N1, picked 27 Oct 2026): the top of the device's Overview is a short to-do
// list, worst first, each problem with a line on why it matters and its own button, in the
// banner language the Overview and the venue page use. Only wireless adb can be switched
// off from here today; the rows whose fix needs a command the clients don't have yet
// (USB debugging, developer options, install rights) open the Health tab instead.

type deviceNeed struct {
	Tone   string // bad | warn
	Title  string
	Why    string
	Action string // "adb" opens the wireless adb dialog, "health" the Health tab, "" none
	Label  string // the button's words
}

func deviceNeedsOf(sec deviceSecurity, d *db.Device, showBattery, flapping bool) []deviceNeed {
	var out []deviceNeed
	if sec.AdbOn {
		out = append(out, deviceNeed{Tone: "bad", Title: "Wireless adb open", Why: "Anyone on the venue Wi-Fi can try to connect", Action: "adb", Label: "Turn off"})
	}
	if sec.UnknownSources == "On" {
		why := "An app may install other apps outside the MDM"
		if len(sec.UnknownApps) > 0 {
			why = strings.Join(sec.UnknownApps, ", ") + " may install apps outside the MDM"
		}
		out = append(out, deviceNeed{Tone: "warn", Title: "Installs from unknown sources allowed", Why: why, Action: "health", Label: "Details"})
	}
	if sec.USBDebugging == "On" {
		out = append(out, deviceNeed{Tone: "warn", Title: "USB debugging on", Why: "A cable gets full access to the device", Action: "health", Label: "Details"})
	}
	if sec.DevOptions == "On" {
		out = append(out, deviceNeed{Tone: "warn", Title: "Developer options on", Why: "Turning them off also turns off USB debugging", Action: "health", Label: "Details"})
	}
	if showBattery && d.BatteryPct < 20 {
		out = append(out, deviceNeed{Tone: "warn", Title: fmt.Sprintf("Battery %d%%", d.BatteryPct), Why: "Put it on its charger"})
	}
	if flapping {
		out = append(out, deviceNeed{Tone: "warn", Title: "Charger flapping", Why: "Charging keeps starting and stopping: check the pad and the cable"})
	}
	return out
}
