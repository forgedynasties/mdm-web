package db

import (
	"context"
	"os"
	"testing"
)

// Roles are set per model (Models, 17 Nov 2026): one role wins over a class pinned to a
// serial, several keep a pin only while it is one of them, and none leaves pins alone.
func TestModelRolesBeatPins(t *testing.T) {
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
	d.pool.Exec(ctx, `INSERT INTO devices (serial_number, agent_kind, product, device_class, class_source) VALUES
		('MR-PIN', 'dpc', 'mr_model', 'kiosk', 'pin'), ('MR-PLAIN', 'dpc', 'mr_model', 'kds', '')`)
	t.Cleanup(func() {
		d.pool.Exec(ctx, `DELETE FROM devices WHERE serial_number LIKE 'MR-%'`)
		d.pool.Exec(ctx, `DELETE FROM product_names WHERE product = 'mr_model'`)
	})
	class := func(s string) (c, src string) {
		d.pool.QueryRow(ctx, `SELECT device_class, class_source FROM devices WHERE serial_number = $1`, s).Scan(&c, &src)
		return
	}
	if _, err := d.SetProductRoles(ctx, "mr_model", []string{"pos", "kiosk"}, "t"); err != nil {
		t.Fatal(err)
	}
	if c, s := class("MR-PIN"); c != "kiosk" || s != "pin" {
		t.Errorf("several roles kept a pin inside them? got %s/%s", c, s)
	}
	if c, _ := class("MR-PLAIN"); c != "" {
		t.Errorf("several roles: a device outside them keeps %q", c)
	}
	if _, err := d.SetProductRoles(ctx, "mr_model", []string{"pos"}, "t"); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"MR-PIN", "MR-PLAIN"} {
		if c, src := class(s); c != "pos" || src != "" {
			t.Errorf("one role: %s is %s/%s, want pos", s, c, src)
		}
	}
	if n, _ := d.ModelRoleMoves(ctx, "mr_model", "kds"); n != 2 {
		t.Errorf("moving to kds would change %d, want 2", n)
	}
}
