package dashboard

import (
	"net/http"
	"sort"
	"strings"

	"mdm/internal/db"
)

// Venues widget (V1, picked 19 Oct 2026): Overview's venue card is the Venues page in
// miniature — one row per venue with its icon, a square per device, its online count and
// problems, and its health score — built from the same rows (collRows), so the two pages
// can never disagree. It replaced the device wall, which showed the same squares without
// names or icons. Venues that need a look come first; ten at most, then a link.

const overviewVenueRows = 10

type overviewVenues struct {
	Rows     []venueRow
	More     int // venues past the first ten
	Total    int // venues shown to this viewer
	NeedLook int
	Unplaced int // visible devices at no venue
}

// overviewVenueData builds the widget. wallDevs is the device list the rest of the
// Overview already loaded; it only supplies the count of devices at no venue.
func (h *Handler) overviewVenueData(r *http.Request, wallDevs []db.WallDevice) overviewVenues {
	acc := h.access(r)
	rows, _ := h.collRows(r, "restaurants")
	keep := rows[:0]
	for _, v := range rows {
		// A scoped viewer sees only the venues they have a device in.
		if acc.hidesDevices() && v.Devices == 0 {
			continue
		}
		keep = append(keep, v)
	}
	unplaced := 0
	for _, d := range wallDevs {
		if d.RestaurantID == nil && (!acc.hidesDevices() || acc.visible(d.ID)) {
			unplaced++
		}
	}
	return venueWidget(keep, unplaced)
}

// venueWidget orders the venue rows for the widget (those needing a look first, then
// by name), counts them and keeps the first ten. Shared with the sneak peek.
func venueWidget(keep []venueRow, unplaced int) overviewVenues {
	out := overviewVenues{Unplaced: unplaced}
	sort.SliceStable(keep, func(i, j int) bool {
		if keep[i].NeedsLook != keep[j].NeedsLook {
			return keep[i].NeedsLook
		}
		return strings.ToLower(keep[i].Name) < strings.ToLower(keep[j].Name)
	})
	out.Total = len(keep)
	for _, v := range keep {
		if v.NeedsLook {
			out.NeedLook++
		}
	}
	if len(keep) > overviewVenueRows {
		out.More, keep = len(keep)-overviewVenueRows, keep[:overviewVenueRows]
	}
	out.Rows = keep
	return out
}
