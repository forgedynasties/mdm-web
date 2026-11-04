package dashboard

import (
	"testing"

	"github.com/google/uuid"

	"mdm/internal/aiobackend"
	"mdm/internal/db"
)

func vnF(v float64) *float64 { return &v }

func TestVenueSearchMatch(t *testing.T) {
	venue := uuid.New()
	s := &vnSources{
		rests: []db.BackendRestaurant{
			{Env: "prod", Restaurant: aiobackend.Restaurant{ID: 1960, Name: "Ole Ole Burrito", City: "Oakland", Latitude: vnF(37.81416), Longitude: vnF(-122.26874)}},
			{Env: "prod", Restaurant: aiobackend.Restaurant{ID: 1994, Name: "Ole Ole Burrito Express", City: "Oakland"}},
			{Env: "stage", Restaurant: aiobackend.Restaurant{ID: 4, Name: "Flights Vegas", City: "Las Vegas", Phone: "+1 (702) 268-7264"}},
			{Env: "prod", Restaurant: aiobackend.Restaurant{ID: 2098, Name: "Dinah's Poolside Restaurant", City: "Palo Alto"}},
		},
		linked: map[string]uuid.UUID{bkKey("prod", 2098): venue},
		names:  map[uuid.UUID]string{venue: "Dinah's Poolside Restaurant"},
		devs:   map[string][]vnDev{bkKey("prod", 1960): {{Serial: "DK1", Label: "Self-order kiosk"}}},
		total:  map[string]int{bkKey("prod", 1960): 2},
	}
	pl := func(name, addr, phone string, lat, lng float64) gPlace {
		var p gPlace
		p.ID, p.DisplayName.Text, p.FormattedAddress, p.NationalPhoneNumber = "g-"+name, name, addr, phone
		if lat != 0 {
			p.Location = &struct {
				Latitude  float64 `json:"latitude"`
				Longitude float64 `json:"longitude"`
			}{lat, lng}
		}
		return p
	}
	out := s.match("ole ole", []gPlace{
		pl("Ole Ole Burrito", "2435 Telegraph Ave, Oakland, CA 94612, USA", "", 37.81420, -122.26880), // ~7 m away
		pl("Flights Las Vegas", "3663 S Las Vegas Blvd, Las Vegas, NV", "(702) 268-7264", 36.1, -115.17),
		pl("Dinah's Poolside Restaurant", "4261 El Camino Real, Palo Alto, CA", "", 0, 0),
	})
	if len(out) != 4 {
		t.Fatalf("want 3 Google rows + 1 backend-only row, got %d: %+v", len(out), out)
	}
	if b := out[0].Backend; b == nil || b.ID != 1960 || len(b.Devices) != 1 || b.Total != 2 {
		t.Errorf("same pin should join backend #1960 with its device: %+v", out[0].Backend)
	}
	if out[0].Address != "2435 Telegraph Ave, Oakland, CA 94612" {
		t.Errorf("address keeps no country: %q", out[0].Address)
	}
	if b := out[1].Backend; b == nil || b.ID != 4 || out[1].Why == "" {
		t.Errorf("same phone should join stage #4 and say the backend's name: %+v %q", b, out[1].Why)
	}
	if v := out[2].Venue; v == nil || v.ID != venue.String() || out[2].Backend == nil {
		t.Errorf("same name in the same city should join #2098, already a venue: %+v", out[2])
	}
	if out[3].Backend == nil || out[3].Backend.ID != 1994 || out[3].Google != nil {
		t.Errorf("backend-only row for #1994: %+v", out[3])
	}
}
