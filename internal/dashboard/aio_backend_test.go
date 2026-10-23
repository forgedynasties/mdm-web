package dashboard

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"mdm/internal/aiobackend"
	"mdm/internal/db"
)

func fp(f float64) *float64 { return &f }

func TestSuggestVenueLinks(t *testing.T) {
	hu := db.Restaurant{ID: uuid.New(), Name: "Huanchaco Peruvian", Latitude: fp(37.9318), Longitude: fp(-121.6958)}
	fl := db.Restaurant{ID: uuid.New(), Name: "Flights Vegas"}
	lab := db.Restaurant{ID: uuid.New(), Name: "AIO Lab"}
	rs := []db.BackendRestaurant{
		{Env: "prod", Restaurant: aiobackend.Restaurant{ID: 2007, Name: "Huanchaco Peruvian Food", Latitude: fp(37.9319), Longitude: fp(-121.6957)}},
		{Env: "prod", Restaurant: aiobackend.Restaurant{ID: 9, Name: "Karahi House"}},
		{Env: "stage", Restaurant: aiobackend.Restaurant{ID: 4, Name: "Flights Vegas"}},
		{Env: "stage", Restaurant: aiobackend.Restaurant{ID: 6, Name: "Flights Vegas Testing"}},
	}
	bds := []db.BackendDevice{
		{Env: "prod", Device: aiobackend.Device{ID: 1, RestaurantID: 2007, Type: "tv", Serial: "ak650aa9200051"}},
		{Env: "prod", Device: aiobackend.Device{ID: 2, RestaurantID: 9, Type: "nugget", Serial: "abc123"}},
	}
	extra, _ := json.Marshal(map[string]any{"app_android_ids": []map[string]string{{"package": "aio.app.nugget", "android_id": "abc123"}}})
	devs := []db.Device{
		{ID: uuid.New(), SerialNumber: "AK650AA9200051", RestaurantID: &hu.ID},
		{ID: uuid.New(), SerialNumber: "AT070AABU00044", RestaurantID: &lab.ID, LatestExtra: extra},
	}
	got := suggestVenueLinks([]db.Restaurant{hu, fl, lab}, devs, rs, bds)

	h := got[hu.ID]
	if len(h) != 1 || h[0].Restaurant.ID != 2007 || h[0].Serials != 1 || h[0].Meters > 50 {
		t.Fatalf("huanchaco: %+v", h)
	}
	if r := h[0].Reasons(); len(r) != 3 || r[0] != "1 device listed there" || r[1] != "same name" {
		t.Fatalf("reasons: %v", r)
	}
	f := got[fl.ID]
	if len(f) != 2 || f[0].Restaurant.ID != 4 || f[0].NameMatch != 1 {
		t.Fatalf("flights: %+v", f)
	}
	l := got[lab.ID]
	if len(l) != 1 || l[0].Restaurant.ID != 9 || l[0].Serials != 1 {
		t.Fatalf("lab via android id: %+v", l)
	}
}

func TestUnenrolledAt(t *testing.T) {
	bds := []db.BackendDevice{
		{Env: "prod", Device: aiobackend.Device{ID: 1, RestaurantID: 2007, Type: "tv", Serial: "AK650AA9200051"}},
		{Env: "prod", Device: aiobackend.Device{ID: 2, RestaurantID: 2007, Type: "tv", Serial: "ADRB0AAK600049"}},
		{Env: "prod", Device: aiobackend.Device{ID: 3, RestaurantID: 2007, Type: "printer", Serial: "P1"}},
		{Env: "prod", Device: aiobackend.Device{ID: 4, RestaurantID: 2007, Type: "kds", Serial: "DK1", Revoked: true}},
		{Env: "stage", Device: aiobackend.Device{ID: 5, RestaurantID: 2007, Type: "pos", Serial: "X"}},
	}
	devs := []db.Device{{SerialNumber: "ak650aa9200051"}}
	got := unenrolledAt(backendKey{"prod", 2007}, bds, devs)
	if len(got) != 1 || got[0].ID != 2 {
		t.Fatalf("got %+v", got)
	}
}

func TestBackendSyncDue(t *testing.T) {
	now := time.Now()
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	cases := []struct {
		s    db.BackendSyncState
		want bool
	}{
		{db.BackendSyncState{}, true},
		{db.BackendSyncState{SyncedAt: ago(time.Hour)}, false},
		{db.BackendSyncState{SyncedAt: ago(25 * time.Hour)}, true},
		{db.BackendSyncState{SyncedAt: ago(time.Hour), Requested: true}, true},
		{db.BackendSyncState{SyncedAt: ago(25 * time.Hour), Error: "x", AttemptedAt: ago(10 * time.Minute)}, false},
		{db.BackendSyncState{Error: "x", AttemptedAt: ago(40 * time.Minute)}, true},
	}
	for i, c := range cases {
		if got := backendSyncDue(c.s, now); got != c.want {
			t.Errorf("case %d: got %v", i, got)
		}
	}
}
