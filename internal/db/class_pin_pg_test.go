package db

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// A class set for one serial (PinDeviceClass) holds only while its model has no role
// (Models, 17 Nov 2026: a model with one role wins over a pin, at check-in and when the
// model changes); UnpinDeviceClass hands it back to the model.
func TestPinnedClassYieldsToModel(t *testing.T) {
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
	sfx := time.Now().Format("150405.000")
	model, serial := "pinmodel"+sfx, "PIN-"+sfx
	extra := json.RawMessage(`{"agent_type":"dpc"}`)
	class := func() (string, string) {
		var c, s string
		if err := d.pool.QueryRow(ctx, `SELECT device_class, class_source FROM devices WHERE serial_number = $1`, serial).Scan(&c, &s); err != nil {
			t.Fatal(err)
		}
		return c, s
	}
	// A model with no role: a pin holds through check-ins.
	if _, _, _, _, _, err := d.UpsertCheckin(ctx, serial, "b1", nil, extra, false, model); err != nil {
		t.Fatal(err)
	}
	if err := d.PinDeviceClass(ctx, serial, "kds"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, err := d.UpsertCheckin(ctx, serial, "b2", nil, extra, false, model); err != nil {
		t.Fatal(err)
	}
	if c, s := class(); c != "kds" || s != "pin" {
		t.Errorf("no model role, after a check-in: %q/%q, want kds/pin", c, s)
	}
	// The model gets one role: it wins, now and at the next check-in.
	if _, err := d.setProductRoles(ctx, model, []string{"mpos"}, "test"); err != nil {
		t.Fatal(err)
	}
	if c, s := class(); c != "mpos" || s != "" {
		t.Errorf("after the model's role: %q/%q, want mpos from the model", c, s)
	}
	if err := d.PinDeviceClass(ctx, serial, "kds"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, err := d.UpsertCheckin(ctx, serial, "b3", nil, extra, false, model); err != nil {
		t.Fatal(err)
	}
	if c, _ := class(); c != "mpos" {
		t.Errorf("a pin against a one-role model, after a check-in: %q, want mpos", c)
	}
	if err := d.UnpinDeviceClass(ctx, serial); err != nil {
		t.Fatal(err)
	}
	if c, s := class(); c != "mpos" || s != "" {
		t.Errorf("unpinned: %q/%q, want mpos from the model", c, s)
	}
	if _, _, _, _, _, err := d.UpsertCheckin(ctx, "PINFW-"+sfx, "b1", nil, json.RawMessage(`{}`), false, "t7"); err != nil {
		t.Fatal(err)
	}
	if err := d.PinDeviceClass(ctx, "PINFW-"+sfx, "pos"); err != ErrRoleNotAllowed {
		t.Errorf("a firmware device was pinned (err %v)", err)
	}
}
