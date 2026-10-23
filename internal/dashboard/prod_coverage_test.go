package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"mdm/internal/aiobackend"
	"mdm/internal/db"
)

func TestBuildProdCoverage(t *testing.T) {
	hu, lab := uuid.New(), uuid.New()
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	rs := []db.BackendRestaurant{
		{Env: "prod", Restaurant: aiobackend.Restaurant{ID: 2007, Name: "Huanchaco Peruvian Food", RecordStatus: "Active"}},
		{Env: "prod", Restaurant: aiobackend.Restaurant{ID: 9, Name: "Redwood Grill", RecordStatus: "Active"}},
		{Env: "prod", Restaurant: aiobackend.Restaurant{ID: 5, Name: "Closed Diner", RecordStatus: "Inactive"}},
		{Env: "prod", Restaurant: aiobackend.Restaurant{ID: 1, Name: "QA Personal v6", RecordStatus: "Active"}},
	}
	bd := func(id, rid int64, typ, serial string) db.BackendDevice {
		return db.BackendDevice{Env: "prod", Device: aiobackend.Device{ID: id, RestaurantID: rid, Type: typ, Serial: serial}}
	}
	bds := []db.BackendDevice{
		bd(1, 2007, "kiosk", "AK650AA9200051"),                                 // placed
		bd(2, 2007, "tv", "ADRB0AAK600049"),                                    // missing
		bd(3, 2007, "nugget", "ca6ba20578e28ae6"),                              // unmatched
		bd(4, 2007, "printer", "printer_x"),                                    // not counted
		bd(5, 9, "kds", "DK1"), bd(6, 9, "pos", "DP1"), bd(7, 9, "pos", "DP2"), // missing ×3
		bd(8, 5, "kds", "DK9"),           // inactive restaurant
		bd(9, 1, "tv", "ADRB0AASI00006"), // elsewhere (no venue)
	}
	devs := []db.Device{
		{ID: uuid.New(), SerialNumber: "AK650AA9200051", RestaurantID: &hu, CreatedAt: now.AddDate(0, 0, -20)},
		{ID: uuid.New(), SerialNumber: "ADRB0AASI00006", CreatedAt: now.AddDate(0, 0, -2)},
		{ID: uuid.New(), SerialNumber: "AT070AABU00044", RestaurantID: &lab},
	}
	links := map[uuid.UUID]db.VenueBackendLink{hu: {Env: "prod", RestaurantID: 2007}}
	v := buildProdCoverage(covInputs{Env: "prod", Rs: rs, Bds: bds, Devs: devs, Links: links,
		Names: map[uuid.UUID]string{hu: "Huanchaco Peruvian"}, Now: now})

	if v.C != (covCounts{Placed: 1, Elsewhere: 1, Missing: 4, Unmatched: 1}) || v.C.Pct() != "29" {
		t.Fatalf("counts %+v pct %s", v.C, v.C.Pct())
	}
	if len(v.Rows) != 3 || v.Rows[0].R.ID != 2007 || v.Rows[2].R.ID != 1 || !v.Rows[2].Test {
		t.Fatalf("rows %+v", v.Rows)
	}
	if v.Linked != 1 || v.WithAny != 2 {
		t.Fatalf("linked %d withAny %d", v.Linked, v.WithAny)
	}
	b := v.Banner
	if len(b.Todos) != 2 || b.Todos[0].Action != "Open venue" || !strings.HasPrefix(b.Todos[1].Title, "Redwood Grill: 3 devices, none in the MDM") ||
		b.Todos[1].Post != "/restaurants/backend-place" {
		t.Fatalf("banner %+v", b.Todos)
	}
	if len(v.Weeks) != 8 || !v.Weeks[7].Cur || v.Weeks[7].Pct != 100 || v.Weeks[5].Pct != 50 {
		t.Fatalf("weeks %+v", v.Weeks)
	}
	// Inactive restaurants counted on request; a type filter narrows everything.
	all := buildProdCoverage(covInputs{Env: "prod", All: true, Rs: rs, Bds: bds, Devs: devs, Links: links, Now: now})
	if all.C.Total() != 8 {
		t.Fatalf("all: %+v", all.C)
	}
	kds := buildProdCoverage(covInputs{Env: "prod", Type: "kds", Rs: rs, Bds: bds, Devs: devs, Links: links, Now: now})
	if kds.C.Total() != 1 || len(kds.Rows) != 1 {
		t.Fatalf("kds: %+v", kds.C)
	}
}
