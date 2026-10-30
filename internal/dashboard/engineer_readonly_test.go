package dashboard

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// TestEngineerReadOnlyAtVenues: an engineer may do everything their role holds on a Lab
// device; on a device at a venue (setting up or in service) only look, screenshot and
// collect logs (user, 30 Oct 2026: "for Engineer, deployed devices should be read only").
func TestEngineerReadOnlyAtVenues(t *testing.T) {
	v := venueV
	lab, setup, live := uuid.New(), uuid.New(), uuid.New()
	a := &access{ctx: context.Background(), role: "engineer", username: "e", scopes: map[uuid.UUID]db.DeviceScope{
		lab: {}, setup: {RestaurantID: &v}, live: {RestaurantID: &v, InService: true},
	}}
	a.once.Do(func() {})
	a.visitOnce.Do(func() { a.visitVenues = map[uuid.UUID]bool{} })
	ceiling := roleCeiling("engineer")
	for _, act := range accessActions {
		if act.Fleet {
			continue
		}
		for _, c := range []struct {
			name string
			dev  uuid.UUID
			want bool
		}{
			{"lab", lab, ceiling[act.Key]},
			{"setting up", setup, ceiling[act.Key] && engineerLookActions[act.Key]},
			{"in service", live, ceiling[act.Key] && engineerLookActions[act.Key]},
		} {
			d := c.dev
			if got := a.decide(act.Key, &d).Allowed; got != c.want {
				t.Errorf("%s on %s: allowed=%v, want %v", act.Key, c.name, got, c.want)
			}
		}
	}
	// The look actions stay open at a venue.
	for _, k := range []string{"view", "screenshot", "query"} {
		d := live
		if !a.decide(k, &d).Allowed {
			t.Errorf("%s at a venue must stay allowed", k)
		}
	}
	// Bulk sends and group changes count the venue devices.
	if n := a.engineerVenueCount("reboot", []uuid.UUID{lab, setup, live}); n != 2 {
		t.Errorf("reboot over lab+setup+live: %d at a venue, want 2", n)
	}
	if n := a.engineerVenueCount("screenshot", []uuid.UUID{setup, live}); n != 0 {
		t.Errorf("screenshots are a look action: %d refused, want 0", n)
	}
	if n := a.engineerVenueCount("groups", []uuid.UUID{lab}); n != 0 {
		t.Errorf("a lab-only group: %d refused, want 0", n)
	}
	other := &access{ctx: context.Background(), role: "operator", scopes: a.scopes}
	other.once.Do(func() {})
	if n := other.engineerVenueCount("reboot", []uuid.UUID{live}); n != 0 {
		t.Errorf("only engineers are counted: %d", n)
	}
	// Alerts: an engineer acts on Lab devices' alerts only.
	alerts := []db.Alert{{ID: uuid.New(), DeviceID: &lab, Type: "device_offline"}, {ID: uuid.New(), DeviceID: &live, Type: "device_offline"}, {ID: uuid.New(), Type: "venue_offline"}}
	if got := a.keepActableAlerts(alerts); len(got) != 1 || *got[0].DeviceID != lab {
		t.Errorf("actable alerts: %+v, want only the lab device's", got)
	}
}

func TestEngineerNeverWrites(t *testing.T) {
	for p, want := range map[string]bool{
		"POST /restaurants":                            true,
		"POST /restaurants/{id}/staff-pin":             true,
		"POST /restaurants/{id}/visits/{vid}":          true,
		"POST /lock/policies":                          true,
		"POST /kiosk/policies/{id}/delete":             true,
		"POST /manage/policies":                        true,
		"POST /devices/bulk-restaurant":                true,
		"POST /commands/clear-attention":               true,
		"POST /devices/{serial}/notes":                 false,
		"POST /groups":                                 false,
		"POST /commands":                               false,
		"POST /apps/register":                          false,
		"POST /devices/bulk-nickname":                  false,
		"POST /releases/{id}/problems":                 false,
	} {
		if got := engineerNeverWrites(p); got != want {
			t.Errorf("%s: %v, want %v", p, got, want)
		}
	}
}
