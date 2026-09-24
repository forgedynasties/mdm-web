package dashboard

import (
	"context"
	"net/http"
	"regexp"

	"mdm/internal/db"
)

// deviceSerialRe mirrors api.validSerial: a real device serial is letters, digits, '-' and
// '_'. Anything else (androidboot.baseband=msm) is a corrupted one that the server refuses.
var deviceSerialRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// refusedCard is one refused source as the roster shows it.
type refusedCard struct {
	db.RefusedCheckin
	Likely string // the restaurant of the latest device reaching us from the same address
}

// refusedCards adds a likely location to each refused source for the roster cards.
func (h *Handler) refusedCards(ctx context.Context, refused []db.RefusedCheckin) []refusedCard {
	out := make([]refusedCard, 0, len(refused))
	for _, s := range refused {
		c := refusedCard{RefusedCheckin: s}
		if near, err := h.db.DevicesAtRemoteIP(ctx, s.RemoteIP, 5); err == nil {
			for _, d := range near {
				if d.Restaurant != "" {
					c.Likely = d.Restaurant
					break
				}
			}
		}
		out = append(out, c)
	}
	return out
}

// refusedSerialPage is /devices/{serial} for a corrupted serial: there is no device record
// (check-ins under it are refused and stored nowhere), so instead of "not found" it shows
// what the serial is, where the tablets reporting it probably are, and how to fix them.
// Returns false when serial is an ordinary one, so the caller's 404 stands.
func (h *Handler) refusedSerialPage(w http.ResponseWriter, r *http.Request, serial string) bool {
	if deviceSerialRe.MatchString(serial) {
		return false
	}
	sources, _ := h.db.RefusedCheckinsForSerial(r.Context(), serial)
	type place struct {
		db.RefusedCheckin
		Nearby []db.DeviceAtAddress
	}
	var places []place
	var attempts int64
	for _, s := range sources {
		near, _ := h.db.DevicesAtRemoteIP(r.Context(), s.RemoteIP, 6)
		places = append(places, place{RefusedCheckin: s, Nearby: near})
		attempts += s.Attempts
	}
	h.render(w, r, "refused_serial.html", map[string]any{
		"Title":    "Corrupted serial",
		"Serial":   serial,
		"Sources":  places,
		"Attempts": attempts,
	})
	return true
}
