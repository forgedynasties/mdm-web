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
		Version: "1.12.0",
		Date:    "2026-06-18",
		Changes: []string{
			"Send a command by filter: the command builder's device picker now has the same filters as the Devices list (status, group, production, release, battery) plus “Select all matching”, so you can target every device that matches.",
			"The device pickers for OTA deploy / add-targets and restaurant assignment gained matching status/battery filters too, so device selection works the same everywhere.",
			"New Logs page: recent logcat captures across the whole fleet, plus your most-used capture presets.",
			"Capture logs from the New Command page — pull logcat from all devices, a group, or specific serials at once, with quick chips for recent and frequent presets.",
			"Retry any past log capture in one click from a device's Logcat page.",
			"Every device serial across the dashboard is now clickable and jumps straight to that device.",
			"Device search boxes now look and behave the same everywhere (and the restaurant device-assignment search filters as you type again).",
			"New Dev account type: full access to releases, OTA, devices and commands, but not settings or user management.",
			"Releases now carry a developer sign-off — a dev marks a build “smoke-tested, OK for QA”, shown as a badge on the Releases list.",
			"Fixed OTA updates that could stay stuck on “pending” after a retry, and a deploy hiccup that could briefly show a 502.",
		},
	},
	{
		Version: "1.11.0",
		Date:    "2026-06-16",
		Changes: []string{
			"Releases page redesigned for a cleaner, more professional look.",
			"Standard test cases are now managed right on the Releases page — the separate Testing and Test cases tabs have been removed.",
			"Only the Tester account can now mark QA results; everyone else sees them read-only.",
			"A release can hold only one full image — the add option is disabled once one exists, so incrementals are added instead.",
			"Removed the per-release adoption chart; the list of devices on each version stays.",
			"Settings → Alerts: alert rules are now split into tabs by category for easier browsing.",
			"A release can now skip the standard test cases, so QA only checks that release's own cases.",
		},
	},
	{
		Version: "1.10.0",
		Date:    "2026-06-16",
		Changes: []string{
			"Test team: a new “Tester” account type and a per-release QA checklist.",
			"Admins keep a library of standard test cases (checked every release) and can add release-specific ones.",
			"Testers mark each case pass/fail/blocked/skip with notes; the release shows the QA summary and warns before publishing if not everything passed.",
			"Uninstall an app from a device directly from its installed-packages list.",
		},
	},
	{
		Version: "1.9.0",
		Date:    "2026-06-16",
		Changes: []string{
			"New device-health alerts: battery wear, app crashes and freezes, weak Wi-Fi, and someone leaving kiosk mode.",
			"Device-offline alerts are now on by default.",
			"Software-update screen: no more flickering, downloads in progress are no longer wrongly marked “stalled”, and it now shows how long each device took plus the deployment average.",
			"Logcat: quick chips to re-run your recent and most-used log requests.",
			"Added this “What's new” page.",
		},
	},
	{
		Version: "1.8.0",
		Date:    "2026-06-15",
		Changes: []string{
			"Restaurants: organise devices by venue, with a searchable picker and bulk assign.",
			"Per-venue service hours, so alerts only fire during opening times.",
			"Rebuilt the Send Command screen — searchable, with duplicate-and-edit and recent/frequent shortcuts.",
		},
	},
	{
		Version: "1.7.0",
		Date:    "2026-06-13",
		Changes: []string{
			"Software updates (OTA): roll out a new build to devices with live progress, retry, and cancel.",
			"Releases: manage build versions and see how many devices are on each.",
			"Renamed the product to AIO MDM.",
		},
	},
	{
		Version: "1.6.0",
		Date:    "2026-06-12",
		Changes: []string{
			"New home overview: fleet status at a glance with key vitals and quick actions.",
			"Refreshed look and feel across the dashboard, with light and dark themes.",
		},
	},
	{
		Version: "1.5.0",
		Date:    "2026-06-11",
		Changes: []string{
			"Security hardening: safer sign-in sessions, login rate-limiting, and stricter request checks.",
			"All assets are now served by the MDM itself (no third-party CDNs).",
		},
	},
	{
		Version: "1.4.0",
		Date:    "2026-06-10",
		Changes: []string{
			"AI fleet report: a plain-language hourly summary of how the fleet is doing.",
			"On-demand AI analysis for a single device, the whole fleet, or settings.",
		},
	},
	{
		Version: "1.3.0",
		Date:    "2026-06-09",
		Changes: []string{
			"Alerts: rules for overheating, charging problems and more, with a dedicated Alerts page.",
			"Get notified in Slack, Discord, or Microsoft Teams when something needs attention.",
			"Fleet Health page scoring each group worst-first.",
		},
	},
	{
		Version: "1.2.0",
		Date:    "2026-06-09",
		Changes: []string{
			"Trends: 30-day battery and charging charts for each device and group.",
		},
	},
	{
		Version: "1.1.0",
		Date:    "2026-06-08",
		Changes: []string{
			"Settings & controls: kill-switches for shell and remote control, command approval rules, and an audit log.",
			"Automatic clean-up of old data on a schedule.",
		},
	},
	{
		Version: "1.0.0",
		Date:    "2026-03-06",
		Changes: []string{
			"First release: monitor devices, view battery and status, and organise them into groups.",
			"Remote control basics — shell, screenshots, reboot, kiosk mode, and app/package inventory.",
			"Logcat and guest charging-pad status.",
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
