package db

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
)

func TestVendorPackagesCountAsSystemAgainstPostgres(t *testing.T) {
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
	var id uuid.UUID
	if err := d.pool.QueryRow(ctx, `INSERT INTO devices (serial_number, device_class) VALUES ('VND-1', 't7') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM devices WHERE serial_number = 'VND-1'`)
	user := false
	if err := d.UpsertDevicePackages(ctx, id, []DevicePackage{
		{PackageName: "com.qti.telephonysettings", AppName: "Telephony", IsSystem: &user},
		{PackageName: "aio.app.nugget", AppName: "Nugget", IsSystem: &user},
	}); err != nil {
		t.Fatal(err)
	}
	pkgs, err := d.GetDevicePackages(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, p := range pkgs {
		got[p.PackageName] = p.EffectiveSystem
	}
	if !got["com.qti.telephonysettings"] || got["aio.app.nugget"] {
		t.Fatalf("effective system = %v; want the vendor package system and ours a user app", got)
	}
	// Every query that classifies packages still runs.
	if _, err := d.SearchFleetPackages(ctx, "nugget"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.PackagesForDevices(ctx, []uuid.UUID{id}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.KioskAppsForDevices(ctx, []uuid.UUID{id}); err != nil {
		t.Fatal(err)
	}
	admin, err := d.ListPackagesAdmin(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range admin {
		if p.PackageName == "com.qti.telephonysettings" && (!p.Vendor || !p.EffectiveSystem) {
			t.Errorf("admin row %+v: want vendor and system", p)
		}
		if p.PackageName == "aio.app.nugget" && p.EffectiveSystem {
			t.Errorf("admin row %+v: ours must stay a user app", p)
		}
	}
}
