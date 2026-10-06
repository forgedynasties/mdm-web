package dashboard

import (
	"errors"
	"fmt"
	"strings"

	"mdm/internal/db"
)

var errNoHygiene = errors.New("clean-up checklist not shown to scoped viewers")

// hygieneRow is one job on the Overview's "Clean up" checklist.
type hygieneRow struct {
	Count int
	What  string
	Note  string
	Href  string
	Link  string
}

// hygieneRows turns the counts into the checklist, leaving out jobs with nothing to do.
func hygieneRows(h db.FleetHygiene) []hygieneRow {
	var out []hygieneRow
	if h.Silent > 0 {
		note := ""
		if h.Fleet > 0 {
			note = fmt.Sprintf("%d%% of the fleet", (h.Silent*200+h.Fleet)/(2*h.Fleet))
		}
		switch {
		case h.SilentPlaced == 0:
			note += ", none in a restaurant. Retire what is gone."
		default:
			note += fmt.Sprintf(", %d still in a restaurant. Retire what is gone.", h.SilentPlaced)
		}
		out = append(out, hygieneRow{h.Silent, "devices silent 14+ days", note, "/devices?hygiene=silent", "Review"})
	}
	if h.PlacedSilent > 0 {
		out = append(out, hygieneRow{h.PlacedSilent, "restaurant devices not reporting",
			strings.Join(h.PlacedSilentAt, ", ") + ". On site, or moved?", "/devices?hygiene=placed-silent", "Review"})
	}
	if h.DeadPad > 0 {
		out = append(out, hygieneRow{h.DeadPad, "dead wireless pads in 14 days", "Replace the pad or the tablet.", "/devices?hygiene=pad", "Review"})
	}
	// One build is the goal; a handful across products and lines is normal. Only
	// builds that are not a release are worth a nudge.
	if n := len(h.OddBuilds); n > 0 {
		names := h.OddBuilds
		if len(names) > 3 {
			names = names[:3]
		}
		note := fmt.Sprintf("%d are not a release: %s", n, strings.Join(names, ", "))
		if n > 3 {
			note += ", …"
		} else {
			note += "."
		}
		out = append(out, hygieneRow{h.Builds, "builds running", note, "/releases", "Releases"})
	}
	return out
}
