package db

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A device moves between two databases: its row and settings arrive, filed as still on the
// old server; a firmware device keeps its key fingerprint, a Standard one gets a token for
// its venue (the same one again on a second import); a venue the target lacks is refused.
func TestDeviceMoveBetweenServers(t *testing.T) {
	base := os.Getenv("MDM_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("MDM_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	src, err := New(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if err := src.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	name := "move_target_" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")
	if _, err := src.pool.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(base)
	u.Path = "/" + name
	dst, err := New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err := dst.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}

	venue, err := src.CreateRestaurant(ctx, Restaurant{Name: "Move venue"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := src.CreateRestaurant(ctx, Restaurant{Name: "Not synced"})
	if err != nil {
		t.Fatal(err)
	}
	// The venue sync copied the venue (same id) to the target.
	if _, err := dst.pool.Exec(ctx, `INSERT INTO restaurants (id, name) VALUES ($1, 'Move venue')`, venue.ID); err != nil {
		t.Fatal(err)
	}

	setup := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)
	var fw, std uuid.UUID
	if err := src.pool.QueryRow(ctx, `INSERT INTO devices (serial_number, agent_kind, device_key_hash, restaurant_id, device_class, last_seen_at)
		VALUES ('MV-FW', 'firmware', 'fw-hash', $1, 't7', NOW() - interval '1 minute') RETURNING id`, venue.ID).Scan(&fw); err != nil {
		t.Fatal(err)
	}
	if err := src.pool.QueryRow(ctx, `INSERT INTO devices (serial_number, agent_kind, device_key_hash, restaurant_id, device_class, class_source, last_seen_at)
		VALUES ('MV-STD', 'dpc', 'std-hash', $1, 'kds', 'pin', NOW()) RETURNING id`, venue.ID).Scan(&std); err != nil {
		t.Fatal(err)
	}
	// MV-FW is in service since two days ago; MV-STD is being set up (placed, not handed over).
	src.pool.Exec(ctx, `UPDATE devices SET handed_over_at = $2, handed_over_by = 'kevin' WHERE id = $1`, fw, setup)
	src.pool.Exec(ctx, `UPDATE devices SET handed_over_at = NULL, handed_over_by = '' WHERE id = $1`, std)
	src.pool.Exec(ctx, `INSERT INTO device_config (device_id, kiosk_enabled, kiosk_package, screen_brightness) VALUES ($1, true, 'aio.app.nugget', 122)`, fw)
	src.pool.Exec(ctx, `INSERT INTO device_nicknames (device_id, name) VALUES ($1, 'Table 4')`, fw)
	src.pool.Exec(ctx, `INSERT INTO device_app_follows (device_id, package, created_by) VALUES ($1, 'aio.app.kds.uatv2', 'qa')`, std)
	src.pool.Exec(ctx, `INSERT INTO hardware_serials (hardware_serial, serial_number) VALUES ('HW-1', 'MV-STD')`)
	src.pool.Exec(ctx, `INSERT INTO device_adb_key (serial, label) VALUES ('MV-STD', 'adbkey-1')`)
	t.Cleanup(func() {
		src.pool.Exec(ctx, `DELETE FROM hardware_serials WHERE serial_number LIKE 'MV-%'`)
		src.pool.Exec(ctx, `DELETE FROM device_adb_key WHERE serial LIKE 'MV-%'`)
		src.pool.Exec(ctx, `DELETE FROM device_moves WHERE serial_number LIKE 'MV-%'`)
		src.pool.Exec(ctx, `DELETE FROM devices WHERE serial_number LIKE 'MV-%'`)
	})

	tok := 0
	newToken := func() string { tok++; return "enr_test" + string(rune('0'+tok)) }
	move := func(serial string) (*DeviceMoveImport, error) {
		b, err := src.ExportDeviceMove(ctx, serial, "https://old.example")
		if err != nil || b == nil {
			t.Fatalf("export %s: %v", serial, err)
		}
		// Through JSON, as over the wire.
		raw, _ := json.Marshal(b)
		var got DeviceMoveBundle
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		return dst.ImportDeviceMove(ctx, &got, "old", "https://old.example", newToken)
	}

	res, err := move("MV-FW")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.EnrollToken != "" || res.Kind != "firmware" {
		t.Errorf("firmware import: %+v", res)
	}
	var hash, custody, source, by, kiosk, nick string
	var handed *time.Time
	var bright int
	dst.pool.QueryRow(ctx, `SELECT COALESCE(device_key_hash, ''), custody_server, custody_source, handed_over_at, handed_over_by FROM devices WHERE serial_number = 'MV-FW'`).
		Scan(&hash, &custody, &source, &handed, &by)
	if hash != "fw-hash" {
		t.Errorf("firmware key fingerprint = %q, want it kept", hash)
	}
	if custody != "old" || source != CustodyMigrate {
		t.Errorf("custody = %q/%q, want old/migrate", custody, source)
	}
	if handed == nil || !handed.Equal(setup) || by != "kevin" {
		t.Errorf("handed over = %v by %q, want %v by kevin", handed, by, setup)
	}
	dst.pool.QueryRow(ctx, `SELECT c.kiosk_package, c.screen_brightness, n.name FROM device_config c JOIN device_nicknames n ON n.device_id = c.device_id JOIN devices d ON d.id = c.device_id WHERE d.serial_number = 'MV-FW'`).
		Scan(&kiosk, &bright, &nick)
	if kiosk != "aio.app.nugget" || bright != 122 || nick != "Table 4" {
		t.Errorf("settings = %q %d %q", kiosk, bright, nick)
	}

	res, err = move("MV-STD")
	if err != nil {
		t.Fatal(err)
	}
	if res.EnrollToken == "" {
		t.Fatal("a Standard device needs a token")
	}
	first := res.EnrollToken
	var stdHash *string
	var placedBy, class string
	var stdHanded *time.Time
	dst.pool.QueryRow(ctx, `SELECT device_key_hash, handed_over_at, handed_over_by, device_class FROM devices WHERE serial_number = 'MV-STD'`).Scan(&stdHash, &stdHanded, &placedBy, &class)
	if stdHash != nil {
		t.Error("a Standard device's key fingerprint must not travel")
	}
	if stdHanded != nil || placedBy != "" {
		t.Errorf("a device being set up stays so, got %v by %q", stdHanded, placedBy)
	}
	if class != "kds" {
		t.Errorf("class = %q", class)
	}
	var profVenue *uuid.UUID
	var maxE int
	dst.pool.QueryRow(ctx, `SELECT restaurant_id, max_enrolls FROM enrollment_profiles WHERE token = $1`, first).Scan(&profVenue, &maxE)
	if profVenue == nil || *profVenue != venue.ID || maxE != 1 {
		t.Errorf("profile venue %v max %d, want the device's venue and 1", profVenue, maxE)
	}
	var follows, hw, adb int
	dst.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM device_app_follows f JOIN devices d ON d.id = f.device_id WHERE d.serial_number = 'MV-STD'),
		(SELECT count(*) FROM hardware_serials WHERE serial_number = 'MV-STD'), (SELECT count(*) FROM device_adb_key WHERE serial = 'MV-STD')`).Scan(&follows, &hw, &adb)
	if follows != 1 || hw != 1 || adb != 1 {
		t.Errorf("follows %d, hardware serials %d, adb keys %d", follows, hw, adb)
	}

	// Again (just before the move goes): the same profile, one more enrolment, the follow
	// list replaced, not doubled.
	dst.pool.Exec(ctx, `UPDATE enrollment_profiles SET enroll_count = 1 WHERE token = $1`, first)
	src.pool.Exec(ctx, `INSERT INTO device_app_follows (device_id, package) VALUES ($1, 'aio.app.kds.stagev2')`, std)
	res, err = move("MV-STD")
	if err != nil {
		t.Fatal(err)
	}
	if res.Created || res.EnrollToken != first {
		t.Errorf("second import: created %v token %q, want the first token", res.Created, res.EnrollToken)
	}
	dst.pool.QueryRow(ctx, `SELECT max_enrolls FROM enrollment_profiles WHERE token = $1`, first).Scan(&maxE)
	dst.pool.QueryRow(ctx, `SELECT count(*) FROM device_app_follows f JOIN devices d ON d.id = f.device_id WHERE d.serial_number = 'MV-STD'`).Scan(&follows)
	if maxE != 2 || follows != 2 {
		t.Errorf("max_enrolls %d follows %d, want 2 and 2", maxE, follows)
	}

	// Custody "migrate" never goes stale by itself.
	dst.pool.Exec(ctx, `UPDATE devices SET custody_set_at = NOW() - interval '30 days', custody_seen_at = NOW() - interval '30 days' WHERE serial_number = 'MV-FW'`)
	dst.ExpireStaleCustody(ctx)
	dst.pool.QueryRow(ctx, `SELECT custody_server FROM devices WHERE serial_number = 'MV-FW'`).Scan(&custody)
	if custody != "old" {
		t.Error("a waiting device lost its custody to the stale sweep")
	}

	// A venue the target doesn't have.
	src.pool.Exec(ctx, `INSERT INTO devices (serial_number, agent_kind, restaurant_id) VALUES ('MV-ELSE', 'dpc', $1)`, other.ID)
	if _, err := move("MV-ELSE"); !errors.As(err, &ErrMoveVenueMissing{}) {
		t.Errorf("unsynced venue: err = %v", err)
	}

	// The standing move on the source.
	if ok, err := src.PutDeviceMove(ctx, "MV-STD", "https://new.example", first, "test"); err != nil || !ok {
		t.Fatalf("put: %v %v", ok, err)
	}
	m, err := src.GetDeviceMove(ctx, "MV-STD")
	if err != nil || m == nil || m.State != MovePending || m.Token() != first || m.Venue != "Move venue" || m.Kind != "dpc" {
		t.Fatalf("move = %+v, %v", m, err)
	}
	cmd := uuid.New()
	src.SetDeviceMoveState(ctx, "MV-STD", MoveSent, "sent", &cmd)
	if ok, _ := src.PutDeviceMove(ctx, "MV-STD", "https://new.example", first, "test"); ok {
		t.Error("a sent move was filed again")
	}
	if ok, _ := src.CancelDeviceMove(ctx, "MV-STD"); ok {
		t.Error("a sent move was canceled")
	}
	m, _ = src.GetDeviceMove(ctx, "MV-STD")
	if m.Attempts != 1 || m.SentAt == nil || m.CommandID == nil || *m.CommandID != cmd {
		t.Errorf("sent move = %+v", m)
	}
	src.SetDeviceMoveState(ctx, "MV-STD", MoveCameBack, "back", nil)
	if ok, _ := src.PutDeviceMove(ctx, "MV-STD", "https://new.example", first, "test"); !ok {
		t.Error("a move that came back can be filed again")
	}
	// No state filter lists every move (a nil slice reaches SQL as NULL).
	if all, err := src.ListDeviceMoves(ctx, nil, nil); err != nil || len(all) == 0 {
		t.Errorf("unfiltered list = %d moves, %v", len(all), err)
	}
	list, err := src.ListDeviceMoves(ctx, []string{MovePending}, &venue.ID)
	if err != nil || len(list) != 1 || list[0].Serial != "MV-STD" {
		t.Errorf("list = %+v, %v", list, err)
	}
}
