package db

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A hidden venue is out of the lists a person sees (ListRestaurants, the rail's health
// query, the counts) but loads by id; its hidden devices stay hidden through a check-in
// (hidden = arm_hidden) while an ordinary hidden device comes back; unhiding restores both;
// the people/lineup export and a device import never carry the hiding to prod.
func TestHiddenVenue(t *testing.T) {
	url := os.Getenv("MDM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MDM_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	d, err := New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	if err := d.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	suffix := strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")
	v, err := d.CreateRestaurant(ctx, Restaurant{Name: "HV venue " + suffix})
	if err != nil {
		t.Fatal(err)
	}
	other, err := d.CreateRestaurant(ctx, Restaurant{Name: "HV other " + suffix})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.pool.Exec(ctx, `DELETE FROM devices WHERE serial_number LIKE 'HV-%'`)
		d.pool.Exec(ctx, `DELETE FROM restaurants WHERE id = ANY($1)`, []uuid.UUID{v.ID, other.ID})
	})
	for _, s := range []string{"HV-1", "HV-2"} {
		if _, _, _, _, _, err := d.UpsertCheckin(ctx, s, "b1", nil, json.RawMessage(`{"agent_type":"dpc","agent_version":"0.15.1"}`), false, ""); err != nil {
			t.Fatal(err)
		}
		if err := d.AssignDeviceToRestaurant(ctx, s, &v.ID); err != nil {
			t.Fatal(err)
		}
	}

	has := func(list []Restaurant, id uuid.UUID) bool {
		for _, r := range list {
			if r.ID == id {
				return true
			}
		}
		return false
	}
	rests, _ := d.ListRestaurants(ctx)
	if !has(rests, v.ID) || !has(rests, other.ID) {
		t.Fatal("venues missing before hiding")
	}

	cands, err := d.ArmCandidates(ctx, []uuid.UUID{v.ID})
	if err != nil || len(cands) != 1 || len(cands[0].Devices) != 2 {
		t.Fatalf("candidates: %+v %v", cands, err)
	}
	if err := d.HideVenue(ctx, v.ID, []string{"HV-1", "HV-2"}); err != nil {
		t.Fatal(err)
	}
	rests, _ = d.ListRestaurants(ctx)
	if has(rests, v.ID) || !has(rests, other.ID) {
		t.Errorf("hidden venue still listed: %v", has(rests, v.ID))
	}
	health, err := d.GetRestaurantHealth(ctx, nil, 7)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range health {
		if g.GroupID == v.ID {
			t.Error("hidden venue in the health rail query")
		}
	}
	got, err := d.GetRestaurant(ctx, v.ID)
	if err != nil || !got.Hidden || got.Name != v.Name {
		t.Fatalf("loading a hidden venue by id: %+v %v", got, err)
	}
	if left, _ := d.ArmCandidates(ctx, []uuid.UUID{v.ID}); len(left) != 0 {
		t.Errorf("a hidden venue's hidden devices are candidates again: %+v", left)
	}

	// A check-in keeps an armed device hidden; a device hidden any other way comes back.
	if _, _, _, _, _, err := d.UpsertCheckin(ctx, "HV-1", "b1", nil, json.RawMessage(`{"agent_type":"dpc"}`), false, ""); err != nil {
		t.Fatal(err)
	}
	var hidden bool
	d.pool.QueryRow(ctx, `SELECT hidden FROM devices WHERE serial_number = 'HV-1'`).Scan(&hidden)
	if !hidden {
		t.Error("a check-in un-hid an armed device")
	}
	d.pool.Exec(ctx, `UPDATE devices SET hidden = TRUE, arm_hidden = FALSE WHERE serial_number = 'HV-2'`)
	if _, _, _, _, _, err := d.UpsertCheckin(ctx, "HV-2", "b1", nil, json.RawMessage(`{"agent_type":"dpc"}`), false, ""); err != nil {
		t.Fatal(err)
	}
	d.pool.QueryRow(ctx, `SELECT hidden FROM devices WHERE serial_number = 'HV-2'`).Scan(&hidden)
	if hidden {
		t.Error("a check-in left an ordinarily hidden device hidden")
	}

	// Hiding never travels: the export says hidden=false, an imported device is plain.
	b, err := d.ExportPeopleLineup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range b.Tables["restaurants"] {
		var row map[string]any
		json.Unmarshal(raw, &row)
		if row["id"] == v.ID.String() && row["hidden"] != false {
			t.Errorf("exported venue hidden = %v", row["hidden"])
		}
	}
	bundle, err := d.ExportDeviceMove(ctx, "HV-1", "http://elsewhere")
	if err != nil || bundle == nil {
		t.Fatal(err)
	}
	if _, err := d.ImportDeviceMove(ctx, bundle, "dev", "http://elsewhere", func() string { return "enr_t" }); err != nil {
		t.Fatal(err)
	}
	var arm bool
	d.pool.QueryRow(ctx, `SELECT hidden, arm_hidden FROM devices WHERE serial_number = 'HV-1'`).Scan(&hidden, &arm)
	if hidden || arm {
		t.Errorf("an imported device carries hiding: hidden=%v arm_hidden=%v", hidden, arm)
	}

	// Unhide.
	d.pool.Exec(ctx, `UPDATE devices SET hidden = TRUE, arm_hidden = TRUE WHERE serial_number = 'HV-1'`)
	n, err := d.UnhideVenues(ctx, []uuid.UUID{v.ID})
	if err != nil || n != 1 {
		t.Fatalf("unhide: %d %v", n, err)
	}
	rests, _ = d.ListRestaurants(ctx)
	if !has(rests, v.ID) {
		t.Error("unhidden venue not listed")
	}
}
