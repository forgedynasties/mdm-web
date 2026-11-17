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
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"mdm/internal/config"
	"mdm/internal/db"
	"mdm/internal/ws"
)

// The move job against a fake prod: a Standard device online goes at once and arrives; a
// firmware device waits for its venue to close, then gets the setprop and a reboot; an
// offline one waits; one that checks in here again came back; a refused move fails.
func TestDeviceMoveJob(t *testing.T) {
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

	// Fake prod: imports answer a token for Standard devices; a device's last check-in
	// there is whatever the test says.
	var mu sync.Mutex
	seenThere := map[string]time.Time{}
	imports := map[string]int{}
	prod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "admin-k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/move/import":
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
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/devices/"):
			s := strings.TrimPrefix(r.URL.Path, "/api/v1/devices/")
			var at *time.Time
			if v, ok := seenThere[s]; ok {
				at = &v
			}
			writeJSON(w, http.StatusOK, map[string]any{"device": map[string]any{"last_seen_at": at}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer prod.Close()
	old := deviceMoveTarget
	deviceMoveTarget = func() string { return prod.URL }
	defer func() { deviceMoveTarget = old }()

	h := &Handler{db: d, hub: ws.NewHub(), cfg: &config.Config{}, adminAPIKey: "admin-k"}

	open, err := d.CreateRestaurant(ctx, db.Restaurant{Name: "Open venue"})
	if err != nil {
		t.Fatal(err)
	}
	closed, err := d.CreateRestaurant(ctx, db.Restaurant{Name: "Closed venue"})
	if err != nil {
		t.Fatal(err)
	}
	// Open all day; closed all day (open == close is never open).
	if err := d.SetServiceWindow(ctx, db.ServiceWindow{RestaurantID: &open.ID, OpenMin: 0, CloseMin: 1439, TZ: "UTC"}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetServiceWindow(ctx, db.ServiceWindow{RestaurantID: &closed.ID, OpenMin: 60, CloseMin: 60, TZ: "UTC"}); err != nil {
		t.Fatal(err)
	}
	add := func(serial, kind string, venue any, seenAgo string) {
		if _, err := pool.Exec(ctx, `INSERT INTO devices (serial_number, agent_kind, restaurant_id, last_seen_at, device_key_hash)
			VALUES ($1, $2, $3, NOW() - $4::interval, $1)`, serial, kind, venue, seenAgo); err != nil {
			t.Fatal(err)
		}
	}
	add("JM-STD", "dpc", open.ID, "1 minute")
	add("JM-FW-OPEN", "firmware", open.ID, "1 minute")
	add("JM-FW-CLOSED", "firmware", closed.ID, "1 minute")
	add("JM-OFF", "dpc", open.ID, "3 hours")
	t.Cleanup(func() {
		pool.Exec(ctx, `DELETE FROM device_moves WHERE serial_number LIKE 'JM-%'`)
		pool.Exec(ctx, `DELETE FROM commands WHERE created_by = 'device move'`)
		pool.Exec(ctx, `DELETE FROM devices WHERE serial_number LIKE 'JM-%'`)
	})

	for _, s := range []string{"JM-STD", "JM-FW-OPEN", "JM-FW-CLOSED", "JM-OFF"} {
		if res := h.startMove(ctx, prod.URL, s, "test"); res.Error != "" || res.State != db.MovePending {
			t.Fatalf("start %s: %+v", s, res)
		}
	}
	h.RunDeviceMoves(ctx)

	get := func(s string) *db.DeviceMove {
		m, err := d.GetDeviceMove(ctx, s)
		if err != nil || m == nil {
			t.Fatalf("%s: %v", s, err)
		}
		return m
	}
	cmdTypes := func(s string) []string {
		rows, _ := pool.Query(ctx, `SELECT c.type FROM commands c JOIN command_targets ct ON ct.command_id = c.id
			JOIN devices dv ON dv.id = ct.target_id WHERE dv.serial_number = $1 ORDER BY c.created_at, c.type DESC`, s)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var x string
			rows.Scan(&x)
			out = append(out, x)
		}
		return out
	}

	if m := get("JM-STD"); m.State != db.MoveSent || m.CommandID == nil {
		t.Fatalf("online Standard device: %+v", m)
	}
	if got := cmdTypes("JM-STD"); len(got) != 1 || got[0] != "move_server" {
		t.Errorf("Standard commands = %v", got)
	}
	var payload string
	pool.QueryRow(ctx, `SELECT payload::text FROM commands WHERE id = $1`, *get("JM-STD").CommandID).Scan(&payload)
	if !strings.Contains(payload, "enr_fake") || !strings.Contains(payload, prod.URL) {
		t.Errorf("move_server payload = %s", payload)
	}
	if m := get("JM-FW-OPEN"); m.State != db.MovePending || !strings.Contains(m.Note, "to close") {
		t.Errorf("firmware at an open venue: %+v", m)
	}
	if m := get("JM-FW-CLOSED"); m.State != db.MoveSent {
		t.Errorf("firmware at a closed venue: %+v", m)
	}
	if got := cmdTypes("JM-FW-CLOSED"); len(got) != 2 || got[0] != "shell" || got[1] != "reboot" {
		t.Errorf("firmware commands = %v", got)
	}
	if m := get("JM-OFF"); m.State != db.MovePending || !strings.Contains(m.Note, "online") {
		t.Errorf("offline device: %+v", m)
	}
	mu.Lock()
	if imports["JM-STD"] != 2 || imports["JM-FW-OPEN"] != 1 {
		t.Errorf("imports = %v, want the sent one imported again just before", imports)
	}
	mu.Unlock()
	var custody, source string
	pool.QueryRow(ctx, `SELECT custody_server, custody_source FROM devices WHERE serial_number = 'JM-STD'`).Scan(&custody, &source)
	if custody != "prod" || source != "move" {
		t.Errorf("custody while moving = %q/%q", custody, source)
	}

	// It checks in on prod: arrived.
	mu.Lock()
	seenThere["JM-STD"] = time.Now().Add(time.Second)
	mu.Unlock()
	h.RunDeviceMoves(ctx)
	if m := get("JM-STD"); m.State != db.MoveArrived || m.ArrivedAt == nil {
		t.Fatalf("after its check-in on prod: %+v", m)
	}
	pool.QueryRow(ctx, `SELECT custody_source FROM devices WHERE serial_number = 'JM-STD'`).Scan(&source)
	if source != db.CustodyMigrate {
		t.Errorf("arrived custody source = %q", source)
	}

	// Later it checks in here again: it came back.
	pool.Exec(ctx, `UPDATE devices SET last_seen_at = NOW() + interval '10 minutes' WHERE serial_number = 'JM-STD'`)
	h.RunDeviceMoves(ctx)
	if m := get("JM-STD"); m.State != db.MoveCameBack {
		t.Errorf("checked in here again: %+v", m)
	}

	// The device refuses the move (a dead token): failed, custody cleared.
	fw := get("JM-FW-CLOSED")
	pool.Exec(ctx, `INSERT INTO command_status (command_id, device_id, status) VALUES ($1, $2, 'failed')`, *fw.CommandID, fw.DeviceID)
	pool.Exec(ctx, `INSERT INTO command_results (command_id, device_id, output) VALUES ($1, $2, 'setprop: permission denied')`, *fw.CommandID, fw.DeviceID)
	h.RunDeviceMoves(ctx)
	if m := get("JM-FW-CLOSED"); m.State != db.MoveFailed || !strings.Contains(m.Note, "permission denied") {
		t.Errorf("refused: %+v", m)
	}
	pool.QueryRow(ctx, `SELECT custody_server FROM devices WHERE serial_number = 'JM-FW-CLOSED'`).Scan(&custody)
	if custody != "" {
		t.Errorf("a failed move keeps custody %q", custody)
	}

	// Offline when the move went out, it collects it 13 minutes later: its acknowledgement
	// is heard here then, which is not coming back (ADRB0AAK600022, 16 Nov 2026).
	add("JM-LATE", "dpc", open.ID, "1 minute")
	if res := h.startMove(ctx, prod.URL, "JM-LATE", "test"); res.Error != "" {
		t.Fatal(res.Error)
	}
	h.RunDeviceMoves(ctx)
	late := get("JM-LATE")
	if late.State != db.MoveSent {
		t.Fatalf("late: %+v", late)
	}
	pool.Exec(ctx, `UPDATE device_moves SET sent_at = NOW() - interval '20 minutes' WHERE serial_number = 'JM-LATE'`)
	pool.Exec(ctx, `INSERT INTO command_status (command_id, device_id, status, updated_at) VALUES ($1, $2, 'completed', NOW() - interval '10 seconds')`, *late.CommandID, late.DeviceID)
	pool.Exec(ctx, `UPDATE devices SET last_seen_at = NOW() - interval '7 seconds' WHERE serial_number = 'JM-LATE'`)
	h.RunDeviceMoves(ctx)
	if m := get("JM-LATE"); m.State != db.MoveSent {
		t.Errorf("a late acknowledgement was taken for coming back: %+v", m)
	}

	// Filed as came back, then the target hears from it more recently than this server: arrived.
	pool.Exec(ctx, `UPDATE device_moves SET state = 'came_back' WHERE serial_number = 'JM-LATE'`)
	mu.Lock()
	seenThere["JM-LATE"] = time.Now().Add(-2 * time.Second)
	mu.Unlock()
	h.RunDeviceMoves(ctx)
	if m := get("JM-LATE"); m.State != db.MoveArrived {
		t.Errorf("a came-back device the target hears from now: %+v", m)
	}
	// One really back here stays so: the target last heard from it before this server did.
	if m := get("JM-STD"); m.State != db.MoveCameBack {
		t.Errorf("JM-STD: %+v", m)
	}

	// Move it now: the firmware device at the open venue goes without waiting for closing.
	if m := get("JM-FW-OPEN"); m.State != db.MovePending {
		t.Fatalf("JM-FW-OPEN before: %+v", m)
	}
	pool.Exec(ctx, `UPDATE devices SET last_seen_at = NOW() WHERE serial_number = 'JM-FW-OPEN'`)
	if ok, err := d.MoveNow(ctx, "JM-FW-OPEN"); err != nil || !ok {
		t.Fatalf("move now: %v %v", ok, err)
	}
	h.RunDeviceMoves(ctx)
	if m := get("JM-FW-OPEN"); m.State != db.MoveSent {
		t.Errorf("moved now: %+v", m)
	}
	if got := cmdTypes("JM-FW-OPEN"); len(got) != 2 || got[0] != "shell" || got[1] != "reboot" {
		t.Errorf("moved now, commands = %v", got)
	}
}
