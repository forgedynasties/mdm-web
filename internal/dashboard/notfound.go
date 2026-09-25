package dashboard

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"mdm/internal/db"
)

// similarSerial is one "did you mean" row on the device-not-found page.
type similarSerial struct {
	Serial     string
	Restaurant string
	Online     bool
	Seen       string // "Last seen 3 days ago", or "" when online / never seen
}

// deviceNotFoundPage is /devices/{serial} for a serial with no device: a page inside the
// dashboard instead of a bare 404, with a search box and the enrolled devices whose serial
// is within two edits of it — the usual cause is a mistyped or cut-off serial. Only
// devices this user may see are suggested, so the page cannot be used to probe serials.
func (h *Handler) deviceNotFoundPage(w http.ResponseWriter, r *http.Request, serial string) {
	var similar []similarSerial
	if len(serial) >= 4 {
		cands, _ := h.db.SerialCandidates(r.Context(), serial, 200)
		similar = h.rankSimilar(r, serial, cands, 5)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	h.render(w, r, "device_not_found.html", map[string]any{
		"Title":   "Device not found",
		"Serial":  serial,
		"Similar": similar,
	})
}

// rankSimilar keeps the visible candidates within two edits of serial, closest first and,
// among equals, the most recently seen first (the order SerialCandidates returns).
func (h *Handler) rankSimilar(r *http.Request, serial string, cands []db.SerialCandidate, limit int) []similarSerial {
	acc := h.access(r)
	online := h.hub.ConnectedIDsForDisplay()
	want := strings.ToUpper(serial)
	type ranked struct {
		similarSerial
		dist int
	}
	var keep []ranked
	for _, c := range cands {
		if !acc.visible(c.ID) {
			continue
		}
		d := editDistance(want, strings.ToUpper(c.Serial))
		if d == 0 || d > 2 {
			continue
		}
		s := similarSerial{Serial: c.Serial, Restaurant: c.Restaurant}
		if _, ok := online[c.ID]; ok {
			s.Online = true
		} else if c.LastSeen != nil {
			s.Seen = "Last seen " + agoText(time.Since(*c.LastSeen))
		}
		keep = append(keep, ranked{s, d})
	}
	sort.SliceStable(keep, func(i, j int) bool { return keep[i].dist < keep[j].dist })
	out := make([]similarSerial, 0, limit)
	for i := 0; i < len(keep) && i < limit; i++ {
		out = append(out, keep[i].similarSerial)
	}
	return out
}

// editDistance is the Levenshtein distance between a and b (bytes: serials are ASCII).
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// agoText is a rough "3 days ago" for the suggestion rows.
func agoText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return countOf(int(d.Minutes()), "minute") + " ago"
	case d < 24*time.Hour:
		return countOf(int(d.Hours()), "hour") + " ago"
	default:
		return countOf(int(d.Hours()/24), "day") + " ago"
	}
}

func countOf(n int, unit string) string { return fmt.Sprintf("%d %s%s", n, unit, plural(n)) }
