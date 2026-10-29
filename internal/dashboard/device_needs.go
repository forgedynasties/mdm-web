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
//
// The Health tab lists the same rows (device tabs plan, 27 Oct 2026): deviceTodoOf merges
// these with its open alerts and failing compliance checks, so the two never disagree.

type deviceNeed struct {
	Tone   string // bad | warn | info
	Title  string
	Why    string
	Action string // "adb" opens the wireless adb dialog, "health" the Health tab, "role" posts a Choice, "" none
	Label  string // the button's words
	// Choices: one button per role for a model that can be several (device_role.go).
	Choices []roleChoice
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

// postureChecks are the compliance checks a posture row already says.
var postureChecks = map[string]string{
	"wireless_adb_off":    "Wireless adb open",
	"adb_off":             "USB debugging on",
	"dev_options_off":     "Developer options on",
	"unknown_sources_off": "Installs from unknown sources allowed",
}

// deviceTodoOf is the device's one to-do list, worst first: critical alerts, its role
// (a model that can be several, waiting for a pick), wireless adb,
// other alerts, the rest of the posture, failing compliance checks a posture row doesn't
// already say, then battery and charger.
func deviceTodoOf(needs []deviceNeed, health deviceHealth) []deviceNeed {
	said := map[string]bool{}
	for _, n := range needs {
		said[n.Title] = true
	}
	var critical, alerts, checks []deviceNeed
	for _, it := range health.Items {
		why := it.Note
		if why == "" {
			why = it.Label
		}
		switch it.Kind {
		case "alert":
			n := deviceNeed{Tone: "warn", Title: it.Title, Why: why, Action: "health", Label: "Open"}
			if it.Tone == "danger" {
				n.Tone = "bad"
				critical = append(critical, n)
			} else {
				alerts = append(alerts, n)
			}
		case "compliance":
			if t, ok := postureChecks[it.CheckID]; ok && said[t] {
				continue
			}
			checks = append(checks, deviceNeed{Tone: "warn", Title: it.Title, Why: why, Action: "health", Label: "Details"})
		}
	}
	var adb, roles, posture, power []deviceNeed
	for _, n := range needs {
		switch {
		case n.Action == "role":
			roles = append(roles, n)
		case n.Action == "adb":
			adb = append(adb, n)
		case n.Action == "":
			power = append(power, n)
		default:
			posture = append(posture, n)
		}
	}
	out := append(critical, roles...)
	out = append(out, adb...)
	out = append(out, alerts...)
	out = append(out, posture...)
	out = append(out, checks...)
	return append(out, power...)
}
