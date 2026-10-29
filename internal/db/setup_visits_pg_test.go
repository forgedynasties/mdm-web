package db

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Device states (roles replan, 30 Oct 2026): placing a device at a venue makes it setting
// up (the trigger), Hand over makes it in service, moving it again resets it, and a visit
// ends by itself once everything at its venue is handed over.
func TestSetupStatesAndVisits(t *testing.T) {
	url := os.Getenv("MDM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MDM_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	d, err := New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.RunMigrations(ctx); err != nil { // idempotent at every start
		t.Fatalf("second migration run: %v", err)
	}
	sfx := time.Now().Format("150405000")
	serial := "SV" + sfx
	id, _, _, _, _, err := d.UpsertCheckin(ctx, serial, "b1", nil, json.RawMessage(`{"agent_type":"dpc"}`), false, "")
	if err != nil {
		t.Fatal(err)
	}
	venue, err := d.CreateRestaurant(ctx, Restaurant{Name: "Visit venue " + sfx})
	if err != nil {
		t.Fatal(err)
	}
	state := func() string {
		sc, err := d.DeviceScopes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return sc[id].State()
	}
	if s := state(); s != StateLab {
		t.Fatalf("new device: %s, want lab", s)
	}
	if err := d.AssignDeviceToRestaurant(ctx, serial, &venue.ID); err != nil {
		t.Fatal(err)
	}
	if s := state(); s != StateSettingUp {
		t.Fatalf("placed: %s, want setting up", s)
	}
	vid, err := d.OpenSetupVisit(ctx, venue.ID, "test", time.Now().Add(72*time.Hour), []string{"field-" + sfx}, "t")
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := d.VisitVenuesFor(ctx, "field-"+sfx); !v[venue.ID] {
		t.Fatal("field person is not on the visit's venue")
	}
	if n, err := d.HandOver(ctx, []uuid.UUID{id}, "t"); err != nil || n != 1 {
		t.Fatalf("hand over: %d %v", n, err)
	}
	if s := state(); s != StateInService {
		t.Fatalf("handed over: %s, want in service", s)
	}
	if v, _ := d.GetSetupVisit(ctx, vid); v.Open() {
		t.Error("the visit stays open after everything at the venue was handed over")
	}
	if err := d.AssignDeviceToRestaurant(ctx, serial, nil); err != nil {
		t.Fatal(err)
	}
	if err := d.AssignDeviceToRestaurant(ctx, serial, &venue.ID); err != nil {
		t.Fatal(err)
	}
	if s := state(); s != StateSettingUp {
		t.Fatalf("placed again: %s, want setting up", s)
	}
}
