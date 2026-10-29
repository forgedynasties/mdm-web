package db

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// One model, several roles (29 Oct 2026): a Galaxy Tab A9+ is a POS terminal or a kitchen
// display. A one-role model forces its role on every check-in as before; with two roles a
// device keeps its own, has none until someone picks, and takes a pick made in AIO Enroll.
func TestModelWithSeveralRoles(t *testing.T) {
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
	sfx := time.Now().Format("150405000")
	model := "mrtab" + sfx
	extra := json.RawMessage(`{"agent_type":"dpc","model":"SM-X210"}`)
	class := func(serial string) (string, string) {
		t.Helper()
		var c, src string
		if err := d.pool.QueryRow(ctx, `SELECT device_class, class_source FROM devices WHERE serial_number = $1`, serial).Scan(&c, &src); err != nil {
			t.Fatal(err)
		}
		return c, src
	}
	checkin := func(serial string) {
		t.Helper()
		if _, _, _, _, _, err := d.UpsertCheckin(ctx, serial, "b1", nil, extra, false, model); err != nil {
			t.Fatal(err)
		}
	}

	// One role: forced, as before.
	if _, err := d.AttachProductRole(ctx, model, "pos", "t"); err != nil {
		t.Fatal(err)
	}
	a, b := "MRA"+sfx, "MRB"+sfx
	checkin(a)
	if c, _ := class(a); c != "pos" {
		t.Fatalf("one-role model: class %q, want pos", c)
	}

	// A second role: devices keep theirs; a new one has none.
	if _, err := d.AttachProductRole(ctx, model, "kds", "t"); err != nil {
		t.Fatal(err)
	}
	if roles, _ := d.ProductRoles(ctx, model); len(roles) != 2 {
		t.Fatalf("roles %v, want pos+kds", roles)
	}
	if r, _ := d.ProductRole(ctx, model); r != "" {
		t.Fatalf("ProductRole of a two-role model = %q, want empty", r)
	}
	checkin(a)
	checkin(b)
	if c, _ := class(a); c != "pos" {
		t.Errorf("existing device lost its role: %q", c)
	}
	if c, _ := class(b); c != "" {
		t.Errorf("new device of a two-role model got %q, want none", c)
	}

	// Picked on the page: kept across check-ins; a role the model lacks is refused.
	if err := d.SetDeviceClass(ctx, b, "kds", "set"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetDeviceClass(ctx, b, "kiosk", "set"); err != ErrRoleNotAllowed {
		t.Errorf("kiosk on a pos/kds model: err %v, want ErrRoleNotAllowed", err)
	}
	checkin(b)
	if c, src := class(b); c != "kds" || src != "set" {
		t.Errorf("after check-in: %q/%q, want kds/set", c, src)
	}

	// Picked in AIO Enroll before enrolling: applied by EnrollDevice.
	id, err := d.CreateEnrollmentProfile(ctx, EnrollmentProfileInput{Name: "mr-" + sfx, Token: "enr_mr" + sfx, CreatedBy: "t"})
	if err != nil {
		t.Fatal(err)
	}
	prof, err := d.GetEnrollmentProfile(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	e := "MRE" + sfx
	if err := d.RequestDeviceRole(ctx, e, "kds", "t"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.EnrollDevice(ctx, prof, e, model, "h"+sfx); err != nil {
		t.Fatal(err)
	}
	if c, src := class(e); c != "kds" || src != "enroll" {
		t.Errorf("enrolled with a pick: %q/%q, want kds/enroll", c, src)
	}
	checkin(e)
	if c, _ := class(e); c != "kds" {
		t.Errorf("enroll pick lost at check-in: %q", c)
	}

	// Detach KDS: one role left, so every device follows it again.
	if _, err := d.DetachProductRole(ctx, model, "kds", "t"); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{a, b, e} {
		if c, src := class(s); c != "pos" || src != "" {
			t.Errorf("%s after detach: %q/%q, want pos/model", s, c, src)
		}
	}
}
