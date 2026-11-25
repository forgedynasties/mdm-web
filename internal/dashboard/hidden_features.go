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
)

func showBatteryData() bool { return batteryDataShown }
func showVenueReport() bool { return venueReportShown }
func showLabButtons() bool  { return labButtonsShown }
