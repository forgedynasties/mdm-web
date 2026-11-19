package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"mdm/internal/config"
	"mdm/internal/db"
	"mdm/internal/ws"
)

type armOut struct {
	Dry    bool           `json:"dry"`
	Totals map[string]int `json:"totals"`
	Venues []struct {
		Venue   string `json:"venue"`
		Skipped string `json:"skipped"`
		Done    bool   `json:"done"`
		Devices []struct {
			Serial string `json:"serial"`
			Action string `json:"action"`
			Error  string `json:"error"`
		} `json:"devices"`
	} `json:"venues"`
}

// Arming: a dry run (the default) changes nothing; confirm=arm files a move for every
// device, hides the venue and its devices, leaves "AIO Lab" alone and is idempotent; unarm
// brings them back. The watcher then sees the hidden devices.
func TestMoveArmAndWatcher(t *testing.T) {
	url := os.Getenv("MDM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MDM_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	d, err := db.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	if err := d.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	var mu sync.Mutex
	imports := map[string]int{}
	prod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/move/import" {
			var b db.DeviceMoveBundle
			json.NewDecoder(r.Body).Decode(&b)
			var dev map[string]any
			json.Unmarshal(b.Device, &dev)
			imports[b.Serial]++
			res := db.DeviceMoveImport{Serial: b.Serial, Kind: dev["agent_kind"].(string)}
			if res.Kind != "firmware" {
				res.EnrollToken = "enr_fake"
			}
			writeJSON(w, http.StatusOK, res)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"device": map[string]any{"last_seen_at": nil}})
	}))
	defer prod.Close()
	old := deviceMoveTarget
	deviceMoveTarget = func() string { return prod.URL }
	defer func() { deviceMoveTarget = old }()

	h := &Handler{db: d, hub: ws.NewHub(), cfg: &config.Config{}, adminAPIKey: "admin-k"}

	us, err := d.CreateRestaurant(ctx, db.Restaurant{Name: "ARM US venue"})
	if err != nil {
		t.Fatal(err)
	}
	lab, err := d.CreateRestaurant(ctx, db.Restaurant{Name: "AIO Lab"})
	if err != nil {
		t.Fatal(err)
	}
	add := func(serial, kind, version string, venue any) {
		if _, err := pool.Exec(ctx, `INSERT INTO devices (serial_number, agent_kind, restaurant_id, last_seen_at, device_key_hash, latest_extra)
			VALUES ($1, $2, $3, NOW() - interval '1 minute', $1, jsonb_build_object('agent_version', $4::text))`, serial, kind, venue, version); err != nil {
			t.Fatal(err)
		}
	}
	add("AR-STD", "dpc", "0.15.1", us.ID)
	add("AR-FW", "firmware", "1.22.3", us.ID)
	add("AR-LAB", "dpc", "0.15.1", lab.ID)
	t.Cleanup(func() {
		pool.Exec(ctx, `DELETE FROM device_moves WHERE serial_number LIKE 'AR-%'`)
		pool.Exec(ctx, `DELETE FROM commands WHERE created_by IN ('device move', 'move-update')`)
		pool.Exec(ctx, `DELETE FROM devices WHERE serial_number LIKE 'AR-%'`)
		pool.Exec(ctx, `DELETE FROM restaurants WHERE id IN ($1, $2)`, us.ID, lab.ID)
	})

	call := func(path string, fn http.HandlerFunc) armOut {
		rec := httptest.NewRecorder()
		fn(rec, httptest.NewRequest(http.MethodPost, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		var out armOut
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	count := func(q string, args ...any) int {
		var n int
		pool.QueryRow(ctx, q, args...).Scan(&n)
		return n
	}
	scope := "?venues=" + us.ID.String() + "," + lab.ID.String()

	// Dry run, by default and with confirm + dry=1: nothing changes.
	for _, p := range []string{"/arm" + scope, "/arm" + scope + "&confirm=arm&dry=1"} {
		out := call(p, h.MoveArm)
		if !out.Dry || out.Totals["to_file"] != 2 {
			t.Fatalf("dry run %s: %+v", p, out)
		}
		if count(`SELECT COUNT(*) FROM device_moves WHERE serial_number LIKE 'AR-%'`) != 0 ||
			count(`SELECT COUNT(*) FROM restaurants WHERE hidden AND id = $1`, us.ID) != 0 ||
			count(`SELECT COUNT(*) FROM devices WHERE hidden AND serial_number LIKE 'AR-%'`) != 0 {
			t.Fatal("a dry run changed something")
		}
	}

	// Armed.
	out := call("/arm"+scope+"&confirm=arm", h.MoveArm)
	if out.Dry || out.Totals["filed"] != 2 || out.Totals["hidden_venues"] != 1 {
		t.Fatalf("arm: %+v", out)
	}
	skipped := false
	for _, v := range out.Venues {
		if v.Venue == "AIO Lab" && v.Skipped == "excluded" {
			skipped = true
		}
	}
	if !skipped {
		t.Error("AIO Lab was not excluded")
	}
	if count(`SELECT COUNT(*) FROM device_moves WHERE serial_number IN ('AR-STD','AR-FW') AND state = 'pending'`) != 2 ||
		count(`SELECT COUNT(*) FROM device_moves WHERE serial_number = 'AR-LAB'`) != 0 {
		t.Error("moves not filed as expected")
	}
	if count(`SELECT COUNT(*) FROM restaurants WHERE hidden AND id = $1`, us.ID) != 1 ||
		count(`SELECT COUNT(*) FROM restaurants WHERE hidden AND id = $1`, lab.ID) != 0 ||
		count(`SELECT COUNT(*) FROM devices WHERE hidden AND arm_hidden AND serial_number IN ('AR-STD','AR-FW')`) != 2 ||
		count(`SELECT COUNT(*) FROM devices WHERE hidden AND serial_number = 'AR-LAB'`) != 0 {
		t.Error("hiding not as expected")
	}
	if count(`SELECT COUNT(*) FROM audit_log WHERE action = 'device.move_arm' AND target = 'ARM US venue'`) != 1 {
		t.Error("no audit entry for the arming")
	}
	// Idempotent: the venue is hidden, nothing in view, nothing filed again.
	again := call("/arm"+scope+"&confirm=arm", h.MoveArm)
	if again.Totals["filed"] != 0 || again.Totals["devices"] != 0 {
		t.Errorf("second arm: %+v", again)
	}
	mu.Lock()
	if imports["AR-STD"] != 1 {
		t.Errorf("imports = %v", imports)
	}
	mu.Unlock()

	// The watcher sees hidden devices. No hook: the gate is open (a Standard device online
	// goes at once).
	h.RunDeviceMoves(ctx)
	if m, _ := d.GetDeviceMove(ctx, "AR-STD"); m == nil || m.State != db.MoveSent || !m.DeviceHidden || !m.VenueHidden {
		t.Fatalf("watcher on a hidden device: %+v", m)
	}

	// Unarm brings the venue and devices back.
	un := httptest.NewRecorder()
	h.MoveUnarm(un, httptest.NewRequest(http.MethodPost, "/unarm?venues="+us.ID.String(), nil))
	if un.Code != http.StatusOK || count(`SELECT COUNT(*) FROM restaurants WHERE hidden AND id = $1`, us.ID) != 0 ||
		count(`SELECT COUNT(*) FROM devices WHERE hidden AND serial_number LIKE 'AR-%'`) != 0 {
		t.Errorf("unarm: %d %s", un.Code, un.Body.String())
	}
	bad := httptest.NewRecorder()
	h.MoveUnarm(bad, httptest.NewRequest(http.MethodPost, "/unarm", nil))
	if bad.Code != http.StatusBadRequest {
		t.Errorf("unarm with no venues = %d", bad.Code)
	}
}

// The watcher's client gate: a device behind the hosted build gets ONE app_update and its
// move waits; when current the move goes; with no hosted build it goes only if the client
// can move itself.
func TestMoveClientGate(t *testing.T) {
	url := os.Getenv("MDM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MDM_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	d, err := db.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	if err := d.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	prod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/import") {
			var b db.DeviceMoveBundle
			json.NewDecoder(r.Body).Decode(&b)
			var dev map[string]any
			json.Unmarshal(b.Device, &dev)
			res := db.DeviceMoveImport{Serial: b.Serial, Kind: dev["agent_kind"].(string)}
			if res.Kind != "firmware" {
				res.EnrollToken = "enr_fake"
			}
			writeJSON(w, http.StatusOK, res)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"device": map[string]any{"last_seen_at": nil}})
	}))
	defer prod.Close()
	old := deviceMoveTarget
	deviceMoveTarget = func() string { return prod.URL }
	defer func() { deviceMoveTarget = old }()

	standing := map[string]db.ClientStanding{}
	var smu sync.Mutex
	h := &Handler{db: d, hub: ws.NewHub(), cfg: &config.Config{}, adminAPIKey: "admin-k"}
	h.SetClientStanding(func(_ context.Context, dev *db.Device) db.ClientStanding {
		smu.Lock()
		defer smu.Unlock()
		return standing[dev.SerialNumber]
	})
	behind := db.ClientStanding{State: "behind", Slot: "dpc", Have: "0.15.1", Want: "0.15.2", CanUpdate: true,
		URL: "http://x/agent/dpc.apk", Payload: json.RawMessage(`{"package":"p","version":"0.15.2","sha256":"s"}`)}

	for _, s := range []string{"CG-BEHIND", "CG-NOBUILD-OLD", "CG-NOBUILD-OK", "CG-FW-NOBUILD"} {
		kind, ver := "dpc", "0.15.1"
		switch s {
		case "CG-NOBUILD-OLD":
			ver = "0.12.9"
		case "CG-FW-NOBUILD":
			kind, ver = "firmware", "1.22.3"
		}
		if _, err := pool.Exec(ctx, `INSERT INTO devices (serial_number, agent_kind, last_seen_at, device_key_hash, latest_extra, hidden, arm_hidden)
			VALUES ($1, $2, NOW(), $1, jsonb_build_object('agent_version', $3::text), TRUE, TRUE)`, s, kind, ver); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		pool.Exec(ctx, `DELETE FROM device_moves WHERE serial_number LIKE 'CG-%'`)
		pool.Exec(ctx, `DELETE FROM commands WHERE created_by IN ('device move', 'move-update')`)
		pool.Exec(ctx, `DELETE FROM devices WHERE serial_number LIKE 'CG-%'`)
	})
	standing["CG-BEHIND"] = behind
	standing["CG-NOBUILD-OLD"] = db.ClientStanding{State: "nobuild", Have: "0.12.9"}
	standing["CG-NOBUILD-OK"] = db.ClientStanding{State: "nobuild", Have: "0.13.4"}
	standing["CG-FW-NOBUILD"] = db.ClientStanding{State: "nobuild", Have: "1.22.3"}
	for s := range standing {
		if res := h.startMove(ctx, prod.URL, s, "test"); res.Error != "" {
			t.Fatal(res.Error)
		}
	}
	updates := func(s string) int {
		var n int
		pool.QueryRow(ctx, `SELECT COUNT(*) FROM commands c JOIN command_targets ct ON ct.command_id = c.id
			JOIN devices dv ON dv.id = ct.target_id WHERE dv.serial_number = $1 AND c.type = 'app_update' AND c.created_by = 'move-update'`, s).Scan(&n)
		return n
	}
	get := func(s string) *db.DeviceMove {
		m, err := d.GetDeviceMove(ctx, s)
		if err != nil || m == nil {
			t.Fatal(s, err)
		}
		return m
	}

	h.RunDeviceMoves(ctx)
	if m := get("CG-BEHIND"); m.State != db.MovePending || !strings.Contains(m.Note, "0.15.1 → 0.15.2") || updates("CG-BEHIND") != 1 {
		t.Errorf("behind: %+v updates=%d", m, updates("CG-BEHIND"))
	}
	// Another minute: still waiting, no second update.
	h.RunDeviceMoves(ctx)
	if get("CG-BEHIND").State != db.MovePending || updates("CG-BEHIND") != 1 {
		t.Errorf("a second update was queued: %d", updates("CG-BEHIND"))
	}
	// Even once the first has left the 20-minute throttle... not yet: it is throttled.
	if m := get("CG-NOBUILD-OLD"); m.State != db.MovePending || !strings.Contains(m.Note, "too old") {
		t.Errorf("old client, no hosted build: %+v", m)
	}
	if m := get("CG-NOBUILD-OK"); m.State != db.MoveSent {
		t.Errorf("0.13.4 client, no hosted build: %+v", m)
	}
	if m := get("CG-FW-NOBUILD"); m.State != db.MoveSent {
		t.Errorf("firmware, no hosted build: %+v", m)
	}
	if updates("CG-NOBUILD-OLD")+updates("CG-NOBUILD-OK") != 0 {
		t.Error("updates queued with no hosted build")
	}
	// The client updated: the move goes.
	smu.Lock()
	standing["CG-BEHIND"] = db.ClientStanding{State: "current", Have: "0.15.2", Want: "0.15.2"}
	smu.Unlock()
	h.RunDeviceMoves(ctx)
	if m := get("CG-BEHIND"); m.State != db.MoveSent {
		t.Errorf("current client: %+v", m)
	}
	// The update failed three times today: the watcher stops asking and applies the minimum.
	if _, err := pool.Exec(ctx, `INSERT INTO devices (serial_number, agent_kind, last_seen_at, device_key_hash, latest_extra, hidden, arm_hidden)
		VALUES ('CG-GIVEUP', 'dpc', NOW(), 'CG-GIVEUP', '{"agent_version":"0.15.1"}', TRUE, TRUE)`); err != nil {
		t.Fatal(err)
	}
	standing["CG-GIVEUP"] = behind
	if res := h.startMove(ctx, prod.URL, "CG-GIVEUP", "test"); res.Error != "" {
		t.Fatal(res.Error)
	}
	dev, _ := d.GetDevice(ctx, "CG-GIVEUP")
	for i := 0; i < 3; i++ {
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO commands (type, apk_url, payload, target_type, created_by) VALUES ('app_update', '', '{}', 'devices', 'move-update') RETURNING id`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		pool.Exec(ctx, `INSERT INTO command_targets (command_id, target_id) VALUES ($1, $2)`, id, dev.ID)
		pool.Exec(ctx, `INSERT INTO command_status (command_id, device_id, status) VALUES ($1, $2, 'failed')`, id, dev.ID)
	}
	h.RunDeviceMoves(ctx)
	if m := get("CG-GIVEUP"); m.State != db.MoveSent {
		t.Errorf("update given up on, client can move itself: %+v", m)
	}
}

func TestVersionAtLeast(t *testing.T) {
	for _, c := range []struct {
		have, min string
		want      bool
	}{{"0.13.4", "0.13.4", true}, {"0.15.1", "0.13.4", true}, {"0.13.3", "0.13.4", false}, {"0.9.1", "0.13.4", false}, {"", "0.13.4", false}, {"1.0", "0.13.4", true}} {
		if got := versionAtLeast(c.have, c.min); got != c.want {
			t.Errorf("versionAtLeast(%q, %q) = %v", c.have, c.min, got)
		}
	}
}
