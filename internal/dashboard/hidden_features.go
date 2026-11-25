package dashboard

// Features hidden from the dashboard on 25 Nov 2026 with their code kept (user: "remove the
// battery data from everywhere", "remove restaurant report", "dont remove code, keep a bool").
// Only the links go: the pages, routes and jobs stay, so true brings each back as it was.
// The device page's Lock buttons are the same kind of switch (lock_buttons.go).
const (
	// batteryDataShown: the Battery data page (/battery) in the More directory and the
	// device page's ⋯ menu.
	batteryDataShown = false
	// venueReportShown: a venue's Weekly report (/restaurants/{id}/report), the button
	// and the ⋯ item on the venue page.
	venueReportShown = false
	// labButtonsShown: "Open in lab" / "Lab view" on the device page (user: "open in lab
	// has to go"; Shell in the ⋯ menu replaces it).
	labButtonsShown = false
	// adbButtonShown: the "adb: on/off" button in the device page's island (user: "do the
	// same with the adb button on device page what you did with the lock button", "keep the
	// wireless adb page tho": the ⋯ menu's Wireless adb… stays and opens the same popover).
	adbButtonShown = false
	// adbMenuShown: "Wireless adb…" in the device page's ⋯ menu (user: "remove wirless adb from
	// hamburger menu"). With the island button also hidden, nothing on the page opens the popover.
	adbMenuShown = false
	// markLostShown: "Mark as lost…" in the device page's ⋯ menu and on left-the-venue alerts
	// (user: "do the same with mark as lost"). Mark as found, for a device already lost, stays.
	markLostShown = false
	// todayAppsTabShown: the Apps tab in the device page's Today card (user: "remove the apps tab"; the
	// dock's Apps entry has the same and more).
	todayAppsTabShown = false
	// allPackagesShown: "all packages" beside the app count in the device page's Apps (user: "remove the
	// all packages button from apps drawer"). The page it opened stays.
	allPackagesShown = false
)

func showBatteryData() bool  { return batteryDataShown }
func showVenueReport() bool  { return venueReportShown }
func showLabButtons() bool   { return labButtonsShown }
func showAdbButton() bool    { return adbButtonShown }
func showAdbMenu() bool      { return adbMenuShown }
func showMarkLost() bool     { return markLostShown }
func showTodayAppsTab() bool { return todayAppsTabShown }
func showAllPackages() bool  { return allPackagesShown }
