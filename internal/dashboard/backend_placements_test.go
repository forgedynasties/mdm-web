package dashboard

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"mdm/internal/aiobackend"
	"mdm/internal/db"
)

func TestBuildPlacements(t *testing.T) {
	lab, hu, fl := uuid.New(), uuid.New(), uuid.New()
	names := map[uuid.UUID]string{lab: "AIO Lab", hu: "Huanchaco Peruvian", fl: "Flights Vegas"}
	dev := func(serial, class string, venue *uuid.UUID) db.Device {
		return db.Device{ID: uuid.New(), SerialNumber: serial, DeviceClass: class, RestaurantID: venue}
	}
	devices := []db.Device{
		dev("ADRB0AAK600026", "dongle", &lab),
		dev("AK650AA9200051", "kiosk", &hu),
		dev("bcf308127809f0b0", "kiosk", nil),
		dev("f8b99e0b509abf89", "kiosk", nil),
		dev("ADRB0AASI00006", "dongle", nil),
		dev("AT070AABU00169", "t7", &lab), // listed at Huanchaco, which is linked
		dev("IGNORED1", "kds", nil),
	}
	rs := []db.BackendRestaurant{
		{Env: "prod", Restaurant: aiobackend.Restaurant{ID: 1942, Name: "Karahi House", City: "San Francisco"}},
		{Env: "prod", Restaurant: aiobackend.Restaurant{ID: 1960, Name: "Ole Ole Burrito", City: "Oakland"}},
		{Env: "prod", Restaurant: aiobackend.Restaurant{ID: 1, Name: "QA Personal v6"}},
		{Env: "prod", Restaurant: aiobackend.Restaurant{ID: 2007, Name: "Huanchaco Peruvian Food"}},
		{Env: "prod", Restaurant: aiobackend.Restaurant{ID: 5, Name: "Ignored Diner"}},
	}
	bd := func(id, rid int64, serial string) db.BackendDevice {
		return db.BackendDevice{Env: "prod", Device: aiobackend.Device{ID: id, RestaurantID: rid, Serial: serial}}
	}
	bds := []db.BackendDevice{
		bd(1, 1942, "ADRB0AAK600026"), bd(2, 1942, "X1"),
		bd(3, 1960, "bcf308127809f0b0"), bd(4, 1960, "f8b99e0b509abf89"), bd(5, 1960, "X2"),
		bd(6, 1, "ADRB0AASI00006"),
		bd(7, 2007, "AK650AA9200051"), bd(8, 2007, "at070aabu00169"),
		bd(9, 5, "IGNORED1"),
	}
	links := map[uuid.UUID]db.VenueBackendLink{hu: {Env: "prod", RestaurantID: 2007}}
	p := buildPlacements(devices, names, links, rs, bds, map[string]bool{"prod/5": true})

	if len(p.Rows) != 3 {
		t.Fatalf("rows: %+v", p.Rows)
	}
	if p.Rows[0].R.ID != 1960 || len(p.Rows[0].Devs) != 2 || p.Rows[0].Total != 3 || p.Rows[0].Summary() != "2 self-order kiosks here have no venue" {
		t.Fatalf("first row: %+v %q", p.Rows[0], p.Rows[0].Summary())
	}
	if p.Rows[1].R.ID != 1942 || p.Rows[1].Summary() != "1 menu board at AIO Lab" {
		t.Fatalf("second row: %q", p.Rows[1].Summary())
	}
	if !p.Rows[2].Test || p.Rows[2].R.ID != 1 {
		t.Fatalf("test restaurant last: %+v", p.Rows[2])
	}
	// Huanchaco is linked: its kiosk is already there; the T7 at AIO Lab is a move.
	if m := p.Moves[hu]; len(m) != 1 || m[0].Serial != "AT070AABU00169" || m[0].Venue != "AIO Lab" {
		t.Fatalf("moves: %+v", p.Moves)
	}
	if p.DeviceVenue["AT070AABU00169"] != hu || len(p.DeviceRest["bcf308127809f0b0"]) != 1 {
		t.Fatal("per-device lookups")
	}
	todo, ok := venueMovesTodo(p.Moves[hu])
	if !ok || todo.Title != "1 device the AIO backend lists here is elsewhere" || !strings.Contains(todo.Sub, "AT070AABU00169 at AIO Lab") || todo.Post != "backend-move" {
		t.Fatalf("todo %+v", todo)
	}
}
