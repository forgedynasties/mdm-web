package dashboard

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/google/uuid"

	"mdm/internal/config"
	"mdm/internal/db"
	"mdm/internal/ratelimit"
	"mdm/internal/ws"
)

// TestMobileAPIAgainstPostgres signs in as a super admin and a viewer and walks the app's
// screens: home, devices, a device, alerts, venues, and a lock the viewer is refused.
func TestMobileAPIAgainstPostgres(t *testing.T) {
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
	cfg, err := config.Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: d, cfg: cfg, hub: ws.NewHub(), loginFails: ratelimit.New(15 * time.Minute)}
	mux := http.NewServeMux()
	h.registerMobileRoutes(mux)

	stamp := time.Now().Format("150405.000")
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw-123456"), bcrypt.MinCost)
	admin, err := d.CreateUser(ctx, "m-admin-"+stamp, string(hash), "admin", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateUser(ctx, "m-viewer-"+stamp, string(hash), "viewer", nil, true); err != nil {
		t.Fatal(err)
	}
	rest, err := d.CreateRestaurant(ctx, db.Restaurant{Name: "Mobile venue " + stamp})
	if err != nil {
		t.Fatal(err)
	}
	serial := "MOB" + strings.ReplaceAll(stamp, ".", "")
	b := 64
	devID, _, _, _, _, err := d.UpsertCheckin(ctx, serial, "v2.1.099", &b,
		json.RawMessage(`{"charging":true,"agent_version":"1.22.1","wifi":"AIO-Office","wifi_rssi":-50,"ip_address":"10.0.0.5","latitude":24.8,"longitude":67.0,"location_accuracy":30}`), false, "t7")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.AssignDeviceToRestaurant(ctx, serial, &rest.ID); err != nil {
		t.Fatal(err)
	}
	// In service (roles replan): placed and handed over, so Sales and Support see it.
	if _, err := d.HandOver(ctx, []uuid.UUID{devID}, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateAlertIfAbsent(ctx, nil, "device_offline", devID, "critical", "went quiet", nil); err != nil {
		t.Fatal(err)
	}

	call := func(method, path, token, body string) (int, map[string]any) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	login := func(user string) string {
		code, out := call("POST", "/api/v1/m/login", "", `{"username":"`+user+`","password":"pw-123456"}`)
		if code != 200 || out["token"] == "" {
			t.Fatalf("login %s: %d %v", user, code, out)
		}
		return out["token"].(string)
	}

	if code, _ := call("GET", "/api/v1/m/home", "", ""); code != 401 {
		t.Fatalf("no token: %d, want 401", code)
	}
	if code, _ := call("POST", "/api/v1/m/login", "", `{"username":"m-admin-`+stamp+`","password":"nope"}`); code != 401 {
		t.Fatalf("bad password: %d", code)
	}
	tok := login(admin.Username)

	code, home := call("GET", "/api/v1/m/home", tok, "")
	if code != 200 || home["stats"] == nil || home["todo"] == nil {
		t.Fatalf("home: %d %v", code, home)
	}
	code, list := call("GET", "/api/v1/m/devices?q="+serial, tok, "")
	if code != 200 || list["total"].(float64) != 1 {
		t.Fatalf("devices: %d %v", code, list)
	}
	row := list["devices"].([]any)[0].(map[string]any)
	if row["battery_pct"].(float64) != 64 || row["charging"] != true || row["venue"].(map[string]any)["id"] != rest.ID.String() {
		t.Fatalf("device row: %v", row)
	}
	code, dev := call("GET", "/api/v1/m/devices/"+serial, tok, "")
	if code != 200 {
		t.Fatalf("device: %d %v", code, dev)
	}
	dd := dev["device"].(map[string]any)
	if dd["wifi"].(map[string]any)["ssid"] != "AIO-Office" || dd["ip"] != "10.0.0.5" || dd["location"] == nil {
		t.Fatalf("device detail: %v", dd)
	}
	if acts := dev["actions"].(map[string]any); acts["reboot"] != true || acts["locate"] != true {
		t.Fatalf("admin actions: %v", acts)
	}
	if len(dev["alerts"].([]any)) != 1 {
		t.Fatalf("device alerts: %v", dev["alerts"])
	}
	code, al := call("GET", "/api/v1/m/alerts?state=open&severity=critical", tok, "")
	if code != 200 || al["counts"].(map[string]any)["open"].(float64) < 1 {
		t.Fatalf("alerts: %d %v", code, al)
	}
	var alertID string
	for _, a := range al["alerts"].([]any) {
		if dv, _ := a.(map[string]any)["device"].(map[string]any); dv != nil && dv["id"] == serial {
			alertID = a.(map[string]any)["id"].(string)
		}
	}
	if alertID == "" {
		t.Fatalf("our alert is not listed: %v", al)
	}
	if code, out := call("POST", "/api/v1/m/alerts/"+alertID+"/snooze", tok, `{"hours":4}`); code != 200 {
		t.Fatalf("snooze: %d %v", code, out)
	}
	if _, al = call("GET", "/api/v1/m/alerts?state=snoozed", tok, ""); len(al["alerts"].([]any)) < 1 {
		t.Fatalf("snoozed list: %v", al)
	}
	code, vs := call("GET", "/api/v1/m/venues", tok, "")
	if code != 200 || len(vs["venues"].([]any)) < 1 {
		t.Fatalf("venues: %d %v", code, vs)
	}
	code, v := call("GET", "/api/v1/m/venues/"+rest.ID.String(), tok, "")
	if code != 200 || len(v["devices"].([]any)) != 1 {
		t.Fatalf("venue: %d %v", code, v)
	}
	if code, _ := call("GET", "/api/v1/m/venues/"+rest.ID.String()+"/icon", tok, ""); code != 404 {
		t.Fatalf("no icon: %d", code)
	}

	// A viewer reads but may not act.
	vt := login("m-viewer-" + stamp)
	_, dev = call("GET", "/api/v1/m/devices/"+serial, vt, "")
	if acts := dev["actions"].(map[string]any); acts["reboot"] == true || acts["unlock"] == true || acts["screenshot"] == true {
		t.Fatalf("viewer actions: %v", acts)
	}
	if code, _ := call("POST", "/api/v1/m/devices/"+serial+"/actions", vt, `{"action":"unlock"}`); code != 403 {
		t.Fatalf("viewer unlock: %d, want 403", code)
	}
	if code, _ := call("POST", "/api/v1/m/alerts/"+alertID+"/ack", vt, ""); code != 403 {
		t.Fatalf("viewer ack: %d, want 403", code)
	}

	// Logout ends the token.
	if code, _ := call("POST", "/api/v1/m/logout", vt, ""); code != 200 {
		t.Fatal("logout")
	}
	if code, _ := call("GET", "/api/v1/m/me", vt, ""); code != 401 {
		t.Fatalf("after logout: %d", code)
	}

	// Microsoft hand-off: the code needs the verifier.
	verifier := strings.Repeat("v", 50)
	sum := sha256.Sum256([]byte(verifier))
	msAppCodes.Store("code-"+stamp, msAppCode{userID: admin.ID, challenge: base64.RawURLEncoding.EncodeToString(sum[:]), exp: time.Now().Add(time.Minute)})
	if code, _ := call("POST", "/api/v1/m/login/microsoft", "", `{"code":"code-`+stamp+`","verifier":"wrong"}`); code != 401 {
		t.Fatalf("wrong verifier: %d", code)
	}
	msAppCodes.Store("code-"+stamp, msAppCode{userID: admin.ID, challenge: base64.RawURLEncoding.EncodeToString(sum[:]), exp: time.Now().Add(time.Minute)})
	if code, out := call("POST", "/api/v1/m/login/microsoft", "", `{"code":"code-`+stamp+`","verifier":"`+verifier+`"}`); code != 200 || out["token"] == nil {
		t.Fatalf("microsoft exchange: %d %v", code, out)
	}
	if code, _ := call("POST", "/api/v1/m/login/microsoft", "", `{"code":"code-`+stamp+`","verifier":"`+verifier+`"}`); code != 401 {
		t.Fatalf("code reused: %d", code)
	}
}
