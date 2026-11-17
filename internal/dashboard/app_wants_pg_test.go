package dashboard

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"mdm/internal/config"
	"mdm/internal/db"
	"mdm/internal/ws"
)

// Apps on a device against Postgres (17 Nov 2026, FW-2026-000066): an added app installs on
// an online device that lacks it and waits for an offline one; Remove uninstalls; nothing is
// sent twice in a row; an app already there is left alone (no version tracking); the role
// app's auto-install leaves a device someone gave another channel; a follow became an add.
func TestAppWantsAgainstPostgres(t *testing.T) {
	url := os.Getenv("MDM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MDM_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	d, err := db.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	cfg, err := config.Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: d, cfg: cfg, hub: ws.NewHub()}

	// The library: POS prod and UAT, and Chrome. Unreachable URLs: apkmeta gives up at once.
	for _, a := range [][3]string{
		{"POS", "http://127.0.0.1:1/pos.apk", "aio.app.pos"},
		{"POS UAT", "http://127.0.0.1:1/pos-uat.apk", "aio.app.pos.uatv2"},
		{"Chrome", "http://127.0.0.1:1/chrome.apk", "com.android.chrome"},
	} {
		if _, err := d.CreateApp(ctx, a[0], a[1], a[2]); err != nil {
			t.Fatal(err)
		}
	}
	add := func(serial, seenAgo string) (id string) {
		if err := pool.QueryRow(ctx, `INSERT INTO devices (serial_number, agent_kind, device_class, last_seen_at)
			VALUES ($1, 'dpc', 'pos', NOW() - $2::interval) RETURNING id`, serial, seenAgo).Scan(&id); err != nil {
			t.Fatal(err)
		}
		// It has reported an app list (with Chrome on it).
		pool.Exec(ctx, `INSERT INTO device_packages (device_id, package_name, version_name) VALUES ($1, 'com.android.chrome', '131')`, id)
		return id
	}
	on := add("AW-ON", "10 seconds")
	off := add("AW-OFF", "3 hours")
	t.Cleanup(func() {
		pool.Exec(ctx, `DELETE FROM commands WHERE created_by = 'app-wants'`)
		pool.Exec(ctx, `DELETE FROM devices WHERE serial_number LIKE 'AW-%'`)
		pool.Exec(ctx, `DELETE FROM apps WHERE apk_url LIKE 'http://127.0.0.1:1/%'`)
	})
	dev := func(serial string) *db.Device {
		x, err := d.GetDevice(ctx, serial)
		if err != nil || x == nil {
			t.Fatalf("%s: %v", serial, err)
		}
		return x
	}
	sent := func(id, typ string) int {
		var n int
		pool.QueryRow(ctx, `SELECT count(*) FROM commands c JOIN command_targets ct ON ct.command_id = c.id
			WHERE ct.target_id = $1 AND c.type = $2 AND c.created_by = 'app-wants'`, id, typ).Scan(&n)
		return n
	}

	for _, s := range []string{"AW-ON", "AW-OFF"} {
		if err := d.SetAppWant(ctx, dev(s).ID, "aio.app.pos.uatv2", db.WantPresent, "qa"); err != nil {
			t.Fatal(err)
		}
		// Chrome is already there: nothing to send for it.
		d.SetAppWant(ctx, dev(s).ID, "com.android.chrome", db.WantPresent, "qa")
	}
	h.RunAppWants(ctx)
	if n := sent(on, "install_apk"); n != 1 {
		t.Errorf("online device: %d installs, want 1", n)
	}
	if n := sent(off, "install_apk"); n != 0 {
		t.Errorf("offline device: %d installs, want none until it's back", n)
	}
	h.RunAppWants(ctx)
	if n := sent(on, "install_apk"); n != 1 {
		t.Errorf("a second run sent it again: %d", n)
	}
	var apk string
	pool.QueryRow(ctx, `SELECT c.apk_url FROM commands c JOIN command_targets ct ON ct.command_id = c.id WHERE ct.target_id = $1 AND c.type = 'install_apk'`, on).Scan(&apk)
	if apk != "http://127.0.0.1:1/pos-uat.apk" {
		t.Errorf("installed %q, want the UAT build", apk)
	}

	// The offline one comes back.
	pool.Exec(ctx, `UPDATE devices SET last_seen_at = NOW() WHERE id = $1`, off)
	h.RunAppWants(ctx)
	if n := sent(off, "install_apk"); n != 1 {
		t.Errorf("back online: %d installs, want 1", n)
	}

	// The view: done once the device lists it (and the install finished); waiting while offline.
	pool.Exec(ctx, `INSERT INTO device_packages (device_id, package_name) VALUES ($1, 'aio.app.pos.uatv2')`, on)
	pool.Exec(ctx, `INSERT INTO command_status (command_id, device_id, status)
		SELECT c.id, $1, 'installed' FROM commands c JOIN command_targets ct ON ct.command_id = c.id
		 WHERE ct.target_id = $1 AND c.type = 'install_apk'`, on)
	if v := h.deviceWantsView(ctx, dev("AW-ON")); len(v) != 0 {
		t.Errorf("box for a device that has everything: %+v", v)
	}
	pool.Exec(ctx, `UPDATE devices SET last_seen_at = NOW() - interval '3 hours' WHERE id = $1`, off)
	pool.Exec(ctx, `DELETE FROM commands WHERE created_by = 'app-wants' AND id IN (SELECT command_id FROM command_targets WHERE target_id = $1)`, off)
	if v := h.deviceWantsView(ctx, dev("AW-OFF")); len(v) != 1 || v[0].State != "waiting" || v[0].Channel != "UAT" {
		t.Errorf("box for the offline device: %+v", v)
	}

	// Remove: uninstalled when online, once; a successful uninstall forgets the package.
	if err := d.SetAppWant(ctx, dev("AW-ON").ID, "com.android.chrome", db.WantAbsent, "qa"); err != nil {
		t.Fatal(err)
	}
	h.RunAppWants(ctx)
	h.RunAppWants(ctx)
	if n := sent(on, "uninstall"); n != 1 {
		t.Errorf("remove: %d uninstalls, want 1", n)
	}
	if v := h.deviceWantsView(ctx, dev("AW-ON")); len(v) != 1 || v[0].State != "removing" {
		t.Errorf("box while removing: %+v", v)
	}
	if err := d.ForgetPackage(ctx, dev("AW-ON").ID, "com.android.chrome"); err != nil {
		t.Fatal(err)
	}
	if v := h.deviceWantsView(ctx, dev("AW-ON")); len(v) != 0 {
		t.Errorf("box after the uninstall: %+v", v)
	}

	// The role app's auto-install leaves a device given another POS channel.
	all, _ := d.AllAppWants(ctx)
	chosen := chosenChannelOf(all)
	if !choseOtherChannel(chosen[dev("AW-ON").ID], "aio.app.pos", "aio.app.pos") {
		t.Error("a device on POS UAT would be handed prod POS")
	}
	if choseOtherChannel(nil, "aio.app.pos", "aio.app.pos") {
		t.Error("a device with nothing added skips its role app")
	}
}
