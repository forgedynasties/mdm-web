package db

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
)

// TestInstallAckNotesAppAgainstPostgres: an install_apk acked installed puts the library
// app into the device's app list at once and clears the inventory hash.
func TestInstallAckNotesAppAgainstPostgres(t *testing.T) {
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
	var dev uuid.UUID
	if err := d.pool.QueryRow(ctx, `INSERT INTO devices (serial_number, device_class, packages_hash) VALUES ('IAK-1', 't7', 'abc') RETURNING id`).Scan(&dev); err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM devices WHERE id = $1`, dev)
	app, err := d.CreateApp(ctx, "Nugget", "", "aio.app.nugget")
	if err != nil {
		t.Fatal(err)
	}
	defer d.pool.Exec(ctx, `DELETE FROM apps WHERE id = $1`, app.ID)
	apk := "https://mdm.example/apps/" + app.ID.String() + "/apk"
	d.pool.Exec(ctx, `UPDATE apps SET version_name = '17.0.0' WHERE id = $1`, app.ID)
	cmd, err := d.CreateCommand(ctx, "install_apk", apk, []byte(`{}`), "devices", []uuid.UUID{dev})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.AckCommand(ctx, cmd.ID, dev, "installed"); err != nil {
		t.Fatal(err)
	}
	var ver, hash string
	if err := d.pool.QueryRow(ctx, `SELECT version_name FROM device_packages WHERE device_id = $1 AND package_name = 'aio.app.nugget'`, dev).Scan(&ver); err != nil || ver != "17.0.0" {
		t.Fatalf("package row = %q, %v", ver, err)
	}
	d.pool.QueryRow(ctx, `SELECT packages_hash FROM devices WHERE id = $1`, dev).Scan(&hash)
	if hash != "" {
		t.Errorf("packages_hash = %q, want cleared", hash)
	}
}
