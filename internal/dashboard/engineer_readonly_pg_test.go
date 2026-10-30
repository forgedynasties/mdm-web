package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/sessions"

	"mdm/internal/config"
	"mdm/internal/db"
	"mdm/internal/ratelimit"
	"mdm/internal/ws"
)

// TestEngineerReadOnlyRoutes: through the real routes, an engineer is refused every write
// that reaches a device at a venue (one device, a bulk send, a group, a venue, an alert)
// and keeps them on Lab devices.
func TestEngineerReadOnlyRoutes(t *testing.T) {
	dsn := os.Getenv("MDM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("MDM_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	d, err := db.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: d, cfg: cfg, hub: ws.NewHub(), loginFails: ratelimit.New(15 * time.Minute),
		store: sessions.NewCookieStore([]byte("0123456789abcdef0123456789abcdef"))}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	stamp := strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")
	eng, err := d.CreateUser(ctx, "eng-ro-"+stamp, "x", "engineer", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	// Leave no engineer behind: the db package's role ladder test re-adds an old role
	// constraint on the same database and fails on any row it doesn't list.
	defer d.DeleteUser(ctx, eng.ID)
	rest, err := d.CreateRestaurant(ctx, db.Restaurant{Name: "RO venue " + stamp})
	if err != nil {
		t.Fatal(err)
	}
	b := 50
	mk := func(serial string) uuid.UUID {
		id, _, _, _, _, err := d.UpsertCheckin(ctx, serial, "v2.1.099", &b, json.RawMessage(`{}`), false, "t7")
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	labSerial, venueSerial := "ROLAB"+stamp, "ROVEN"+stamp
	labID, venueID := mk(labSerial), mk(venueSerial)
	if err := d.AssignDeviceToRestaurant(ctx, venueSerial, &rest.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.HandOver(ctx, []uuid.UUID{venueID}, "test"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{labID, venueID} {
		if _, err := d.CreateAlertIfAbsent(ctx, nil, "device_offline", id, "warning", "quiet", nil); err != nil {
			t.Fatal(err)
		}
	}
	alertOf := func(dev uuid.UUID) string {
		as, err := d.ListDeviceActiveAlerts(ctx, dev, 5)
		if err != nil || len(as) == 0 {
			t.Fatalf("no alert for %s: %v", dev, err)
		}
		return as[0].ID.String()
	}

	post := func(path string, form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req = req.WithContext(context.WithValue(req.Context(), ctxSessionKey{}, &db.Session{
			UserID: &eng.ID, Username: eng.Username, Role: "engineer",
			LastSeen: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	refused := func(name, path string, form url.Values) {
		t.Helper()
		if rec := post(path, form); rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d %q, want 403", name, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}
	allowed := func(name, path string, form url.Values) {
		t.Helper()
		if rec := post(path, form); rec.Code == http.StatusForbidden || rec.Code == http.StatusNotFound {
			t.Errorf("%s: %d %q, want it through", name, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}
	v := func(kv ...string) url.Values {
		out := url.Values{}
		for i := 0; i+1 < len(kv); i += 2 {
			out.Add(kv[i], kv[i+1])
		}
		return out
	}

	// One device.
	refused("notes at a venue", "/devices/"+venueSerial+"/notes", v("notes", "x"))
	refused("lock at a venue", "/devices/"+venueSerial+"/kiosk", v("kiosk_enabled", "0"))
	refused("charging pad at a venue", "/devices/"+venueSerial+"/wlc", v("wlc_charging_enabled", "0"))
	refused("reboot at a venue", "/devices/"+venueSerial+"/commands", v("type", "reboot"))
	refused("follow at a venue", "/devices/"+venueSerial+"/follows", v("action", "follow", "package", "aio.app.kiosk.qa"))
	// Bulk: one venue device in the selection refuses the whole send.
	refused("bulk reboot incl. a venue device", "/commands", v("type", "reboot", "target_type", "devices", "target_serials", labSerial+"\n"+venueSerial))
	refused("bulk lock at a venue", "/devices/bulk-kiosk", v("serials", venueSerial, "kiosk_enabled", "0"))
	// Groups.
	refused("group with a venue device", "/groups", v("name", "RO mixed "+stamp, "serials", venueSerial))
	allowed("group of lab devices", "/groups", v("name", "RO lab "+stamp, "serials", labSerial))
	g, err := d.CreateGroup(ctx, "RO venue group "+stamp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.AddDevicesToGroup(ctx, []string{venueSerial}, g.ID); err != nil {
		t.Fatal(err)
	}
	refused("rename a group holding a venue device", "/groups/"+g.ID.String(), v("name", "renamed"))
	refused("delete a group holding a venue device", "/groups/"+g.ID.String()+"/delete", v())
	refused("add a lab device to a group holding a venue device", "/groups/"+g.ID.String()+"/devices", v("serials", labSerial))
	// Venues and lock rules.
	refused("rename a venue", "/restaurants/"+rest.ID.String()+"/rename", v("name", "x"))
	refused("venue staff PIN", "/restaurants/"+rest.ID.String()+"/staff-pin", v("pin", "123456"))
	refused("new venue", "/restaurants", v("name", "RO new "+stamp))
	refused("lock rule", "/lock/policies", v("name", "x"))
	// Alerts.
	refused("ack a venue device's alert", "/alerts/"+alertOf(venueID)+"/ack", v())
	refused("problem action on a venue device's alert", "/alerts/problem", v("op", "ack", "ids", alertOf(venueID)))
	allowed("ack a lab device's alert", "/alerts/"+alertOf(labID)+"/ack", v())
}
