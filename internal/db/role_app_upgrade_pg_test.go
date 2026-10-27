package db

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
)

// Auto-upgrade is on by default and has its own switch (27 Oct 2026).
func TestRoleAppAutoUpgradeAgainstPostgres(t *testing.T) {
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
	defer d.pool.Exec(ctx, `DELETE FROM role_apps WHERE role = 'payment'`)
	if _, err := d.pool.Exec(ctx, `INSERT INTO role_apps (role, base_package) VALUES ('payment', 'aio.app.pay')`); err != nil {
		t.Fatal(err)
	}
	get := func() RoleApp {
		apps, err := d.ListRoleApps(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range apps {
			if a.Role == "payment" {
				return a
			}
		}
		t.Fatal("role missing")
		return RoleApp{}
	}
	if a := get(); !a.AutoUpgrade || !a.AutoInstall {
		t.Fatalf("defaults = %+v", a)
	}
	if err := d.SetRoleAppAutoUpgrade(ctx, "payment", false, "test"); err != nil {
		t.Fatal(err)
	}
	if a := get(); a.AutoUpgrade || !a.AutoInstall || a.BasePackage != "aio.app.pay" {
		t.Fatalf("after off = %+v", a)
	}
}

func TestRefusedInstallsOfAgainstPostgres(t *testing.T) {
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
	defer d.pool.Exec(ctx, `DELETE FROM devices WHERE serial_number LIKE 'REFUSE-%'`)
	mk := func(sn, out string) uuid.UUID {
		var id uuid.UUID
		if err := d.pool.QueryRow(ctx, `INSERT INTO devices (serial_number) VALUES ($1) RETURNING id`, sn).Scan(&id); err != nil {
			t.Fatal(err)
		}
		c, err := d.CreateCommand(ctx, "install_apk", "https://lib/nugget-18.apk", []byte(`{}`), "devices", []uuid.UUID{id})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.pool.Exec(ctx, `INSERT INTO command_results (command_id, device_id, output) VALUES ($1, $2, $3)`, c.ID, id, out); err != nil {
			t.Fatal(err)
		}
		return id
	}
	bad := mk("REFUSE-1", "Failure [INSTALL_FAILED_UPDATE_INCOMPATIBLE: Existing package aio.app.nugget signatures do not match]")
	cut := mk("REFUSE-2", "error: ProtocolException: unexpected end of stream")
	got, err := d.RefusedInstallsOf(ctx, []uuid.UUID{bad, cut}, []string{"https://lib/nugget-18.apk"})
	if err != nil {
		t.Fatal(err)
	}
	if !got[bad] || got[cut] {
		t.Errorf("refused = %v", got)
	}
}
