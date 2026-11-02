package dashboard

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// TestEngineerReadOnlyAtVenues: with the seeded levels (access by levels, 2 Nov 2026) an
// engineer operates Lab devices (Operate: no remote control, Wi-Fi or moving devices) and
// only looks at, screenshots and collects logs from devices at a venue.
func TestEngineerReadOnlyAtVenues(t *testing.T) {
	v := venueV
	lab, setup, live := uuid.New(), uuid.New(), uuid.New()
	a := &access{ctx: context.Background(), role: "engineer", username: "e", scopes: map[uuid.UUID]db.DeviceScope{
		lab: {}, setup: {RestaurantID: &v}, live: {RestaurantID: &v, InService: true},
	}}
	a.once.Do(func() {})
	a.visitOnce.Do(func() { a.visitVenues = map[uuid.UUID]bool{} })
	a.lvls = seedLevels("engineer")
	a.lvlOnce.Do(func() {})
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
			{"lab", lab, ceiling[act.Key] && needLevel(act.Key) <= db.LevelOperate},
			{"setting up", setup, ceiling[act.Key] && needLevel(act.Key) <= db.LevelView},
			{"in service", live, ceiling[act.Key] && needLevel(act.Key) <= db.LevelView},
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
	// Bulk sends count the devices their level is too low for.
	if n := a.engineerVenueCount("reboot", []uuid.UUID{lab, setup, live}); n != 2 {
		t.Errorf("reboot over lab+setup+live: %d refused, want 2", n)
	}
	if n := a.engineerVenueCount("screenshot", []uuid.UUID{setup, live}); n != 0 {
		t.Errorf("screenshots are a look action: %d refused, want 0", n)
	}
	// Moving devices between groups is Manage: Lab's Operate doesn't reach it.
	if n := a.engineerVenueCount("groups", []uuid.UUID{lab}); n != 1 {
		t.Errorf("a lab-only group change: %d refused, want 1 (Manage)", n)
	}
	other := &access{ctx: context.Background(), role: "operator", scopes: a.scopes}
	other.once.Do(func() {})
	if n := other.engineerVenueCount("reboot", []uuid.UUID{live}); n != 0 {
		t.Errorf("only engineers are counted: %d", n)
	}
	// Alerts: an engineer acts on the alerts of devices they may operate.
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
