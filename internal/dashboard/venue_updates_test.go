package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"mdm/internal/config"
	"mdm/internal/db"
)

func TestVenueGateAllows(t *testing.T) {
	venue, other := uuid.New(), uuid.New()
	dev := func(rid *uuid.UUID) *db.Device {
		return &db.Device{RestaurantID: rid, LatestExtra: json.RawMessage(`{"timezone":"UTC"}`)}
	}
	fleet := config.ClientAutoUpdate{On: true, WindowStart: "02:00", WindowEnd: "05:00"}
	// Service hours 11:00–23:00 UTC.
	sw := db.ServiceWindow{OpenMin: 660, CloseMin: 1380, TZ: "UTC"}
	later := time.Now().Add(48 * time.Hour)
	earlier := time.Now().Add(-time.Hour)
	g := venueGate{
		pols:    map[uuid.UUID]db.VenueUpdatePolicy{},
		windows: map[uuid.UUID]db.ServiceWindow{venue: sw},
		fleetSW: db.ServiceWindow{OpenMin: 420, CloseMin: 1380},
	}
	nowUTC := time.Now().UTC()
	h := nowUTC.Hour()
	inFleet := h >= 2 && h < 5
	open := h >= 11 && h < 23

	if got := g.allows(dev(nil), fleet, nowUTC); got != inFleet {
		t.Errorf("no venue: %v, want the fleet window (%v)", got, inFleet)
	}
	if got := g.allows(dev(&other), fleet, nowUTC); got != inFleet {
		t.Errorf("venue with no setting: %v, want the fleet window", got)
	}
	g.pols[venue] = db.VenueUpdatePolicy{Mode: db.VenueUpdateClosed}
	if got := g.allows(dev(&venue), fleet, nowUTC); got != !open {
		t.Errorf("while closed at %02d UTC: %v, want %v", h, got, !open)
	}
	g.pols[venue] = db.VenueUpdatePolicy{Mode: db.VenueUpdateOwn, Start: "00:00", End: "23:59"}
	if !g.allows(dev(&venue), fleet, nowUTC) && nowUTC.Format("15:04") < "23:59" {
		t.Error("own window 00:00–23:59 refused")
	}
	g.pols[venue] = db.VenueUpdatePolicy{Mode: db.VenueUpdateOwn, Start: "00:00", End: "23:59", Paused: true, PausedUntil: &later}
	if g.allows(dev(&venue), fleet, nowUTC) {
		t.Error("a paused venue was allowed")
	}
	g.pols[venue] = db.VenueUpdatePolicy{Mode: db.VenueUpdateOwn, Start: "00:00", End: "23:59", Paused: true}
	if g.allows(dev(&venue), fleet, nowUTC) {
		t.Error("a venue paused until resumed was allowed")
	}
	g.pols[venue] = db.VenueUpdatePolicy{Mode: db.VenueUpdateOwn, Start: "00:00", End: "23:59", Paused: true, PausedUntil: &earlier}
	if !g.allows(dev(&venue), fleet, nowUTC) && nowUTC.Format("15:04") < "23:59" {
		t.Error("an ended pause still held")
	}
	// No closed hours (open == close): while closed falls back to the fleet window.
	g.windows[venue] = db.ServiceWindow{OpenMin: 600, CloseMin: 600}
	g.pols[venue] = db.VenueUpdatePolicy{Mode: db.VenueUpdateClosed}
	if got := g.allows(dev(&venue), fleet, nowUTC); got != inFleet {
		t.Errorf("while closed with no closed hours: %v, want the fleet window", got)
	}
}

func TestVenuePauseTodo(t *testing.T) {
	if _, ok := venuePauseTodo(venueUpdatesView{}); ok {
		t.Error("a venue that isn't paused got a banner line")
	}
	td, ok := venuePauseTodo(venueUpdatesView{Paused: true, PausedSub: "resumes Fri 24 Oct, 09:00", By: "by ali · 22 Oct"})
	if !ok || td.Title != "Updates paused here" || td.Action != "Resume" || !strings.Contains(td.Sub, "by ali · 22 Oct · resumes Fri 24 Oct, 09:00") {
		t.Errorf("banner line = %+v", td)
	}
}
