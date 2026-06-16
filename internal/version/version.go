// Package version is the single source of truth for the MDM server version and its
// user-facing changelog. To cut a release, add a new Entry at the TOP of Changelog:
// the first entry's Version is the live server version shown in the dashboard.
//
// Versioning is SemVer-lite: MAJOR.MINOR.PATCH — bump MINOR for new features, PATCH
// for fixes, MAJOR only for big/breaking changes. Keep Changes brief and in plain
// language a non-technical user can follow.
package version

// Entry is one released version and its plain-language change list.
type Entry struct {
	Version string
	Date    string // YYYY-MM-DD
	Changes []string
}

// Changelog is newest-first. The top entry is the current server version.
var Changelog = []Entry{
	{
		Version: "1.1.0",
		Date:    "2026-06-16",
		Changes: []string{
			"New device health alerts: battery wear, app crashes and freezes, weak Wi-Fi, and kiosk exits.",
			"Device-offline alerts are now on by default.",
			"Cleaner software-update screen — no more flickering, and downloads in progress are no longer wrongly marked “stalled”.",
		},
	},
	{
		Version: "1.0.0",
		Date:    "2026-06-01",
		Changes: []string{
			"First release of the MDM dashboard: device monitoring, groups, restaurants, software updates, and alerts.",
		},
	},
}

// Current returns the live server version (the newest changelog entry).
func Current() string {
	if len(Changelog) == 0 {
		return "0.0.0"
	}
	return Changelog[0].Version
}
