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
	"github.com/jackc/pgx/v5/pgxpool"

	"mdm/internal/config"
	"mdm/internal/db"
	"mdm/internal/otagate"
	"mdm/internal/ratelimit"
	"mdm/internal/ws"
)

// "Why did it fail?" against Postgres: install rights may ask (one fixed read-only logcat
// command, audited, recorded), a viewer may not, and the device's answer is phrased when the
// command page is drawn.
func TestInstallWhyAgainstPostgres(t *testing.T) {
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
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	cfg, err := config.Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: d, cfg: cfg, hub: ws.NewHub(), loginFails: ratelimit.New(15 * time.Minute), otaGate: otagate.New(d, cfg),
		store: sessions.NewCookieStore([]byte("0123456789abcdef0123456789abcdef"))}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	stamp := strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")
	serial := "IW" + stamp
	pkg := "aio.app.camera" + stamp[:3]
	apk := "http://127.0.0.1:1/iw-" + stamp + ".apk"
	b := 50
	if _, _, _, _, _, err := d.UpsertCheckin(ctx, serial, "v2.1.099", &b, json.RawMessage(`{}`), false, "kiosk22"); err != nil {
		t.Fatal(err)
	}
	dev, err := d.GetDevice(ctx, serial)
	if err != nil || dev == nil {
		t.Fatal(err)
	}
	// In service (placed at a venue): operators act only on those.
	rest, err := d.CreateRestaurant(ctx, db.Restaurant{Name: "IW venue " + stamp})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE devices SET restaurant_id = $2 WHERE id = $1`, dev.ID, rest.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateApp(ctx, "Camera "+stamp, apk, pkg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Exec(ctx, `DELETE FROM commands WHERE id IN (SELECT command_id FROM command_targets WHERE target_id = $1)`, dev.ID)
		pool.Exec(ctx, `DELETE FROM devices WHERE id = $1`, dev.ID)
		pool.Exec(ctx, `DELETE FROM apps WHERE apk_url = $1`, apk)
	})
	if !dev.Supports("shell") {
		t.Skip("the test device's agent has no shell capability")
	}

	// A failed install with the generic text.
	failed, err := d.CreateCommandBy(ctx, "install_apk", apk, json.RawMessage(`{}`), "devices", []uuid.UUID{dev.ID}, "qa")
	if err != nil {
		t.Fatal(err)
	}
	pool.Exec(ctx, `INSERT INTO command_status (command_id, device_id, status) VALUES ($1, $2, 'failed')
		ON CONFLICT (command_id, device_id) DO UPDATE SET status = 'failed'`, failed.ID, dev.ID)
	if err := d.SaveCommandResult(ctx, failed.ID, dev.ID, "install failed: status=1 legacy=-999"); err != nil {
		t.Fatal(err)
	}

	post := func(role, path string, form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		uid := uuid.New()
		req = req.WithContext(context.WithValue(req.Context(), ctxSessionKey{}, &db.Session{
			UserID: &uid, Username: "iw-" + role, Role: role,
			LastSeen: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	nShell := func() (n int) {
		pool.QueryRow(ctx, `SELECT count(*) FROM commands c JOIN command_targets ct ON ct.command_id = c.id
			WHERE ct.target_id = $1 AND c.type = 'shell'`, dev.ID).Scan(&n)
		return
	}
	form := url.Values{"command": {failed.ID.String()}}
	path := "/devices/" + serial + "/install-why"

	if rec := post("viewer", path, form); rec.Code != http.StatusForbidden || nShell() != 0 {
		t.Fatalf("viewer: %d, shell commands %d", rec.Code, nShell())
	}
	// Offline: nothing is queued.
	pool.Exec(ctx, `UPDATE devices SET last_seen_at = NOW() - interval '3 hours' WHERE id = $1`, dev.ID)
	if rec := post("admin", path, form); rec.Code != http.StatusConflict || nShell() != 0 {
		t.Fatalf("offline: %d, shell commands %d", rec.Code, nShell())
	}
	pool.Exec(ctx, `UPDATE devices SET last_seen_at = NOW() WHERE id = $1`, dev.ID)
	// A package nobody failed to install is refused (no command text from the form either way).
	if rec := post("admin", path, url.Values{"package": {"x;reboot"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad package: %d", rec.Code)
	}
	if rec := post("operator", path, form); rec.Code != http.StatusNoContent {
		t.Fatalf("operator: %d %s", rec.Code, rec.Body.String())
	}
	if nShell() != 1 {
		t.Fatalf("want one shell command, got %d", nShell())
	}
	var payload json.RawMessage
	var by string
	pool.QueryRow(ctx, `SELECT c.payload, c.created_by FROM commands c JOIN command_targets ct ON ct.command_id = c.id
		WHERE ct.target_id = $1 AND c.type = 'shell'`, dev.ID).Scan(&payload, &by)
	want, _ := diagnoseInstallCommand(pkg)
	var got struct{ Cmd string }
	json.Unmarshal(payload, &got)
	if got.Cmd != want || by != "iw-operator" {
		t.Fatalf("command %q by %q", got.Cmd, by)
	}
	var audits int
	pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'command.diagnose_install' AND target = $1 AND detail LIKE $2`, pkg, "device="+serial+",%").Scan(&audits)
	if audits != 1 {
		t.Fatalf("audit rows %d", audits)
	}

	// Asked, not answered: the page says so and offers no second button.
	ds := []db.CommandDelivery{{DeviceID: dev.ID, SerialNumber: serial, Status: "failed", Output: "install failed: status=1 legacy=-999", Online: true}}
	req := httptest.NewRequest("GET", "/", nil)
	uid := uuid.New()
	req = req.WithContext(context.WithValue(req.Context(), ctxSessionKey{}, &db.Session{UserID: &uid, Username: "iw-operator", Role: "operator", LastSeen: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}))
	fc, _ := d.GetCommand(ctx, failed.ID)
	if v := h.commandInstallWhys(ctx, req, fc, ds)[serial]; !v.Asking || v.CanAsk {
		t.Fatalf("after asking: %+v", v)
	}

	// The device answers with the real log lines.
	var diagID uuid.UUID
	pool.QueryRow(ctx, `SELECT diag_command_id FROM install_diagnoses WHERE device_id = $1`, dev.ID).Scan(&diagID)
	pool.Exec(ctx, `INSERT INTO command_results (command_id, device_id, output) VALUES ($1, $2, $3)`, diagID, dev.ID,
		"11-19 10:22:01.123  1000  812  830 E PackageInstallerSession: Marking session 7 as failed: INSTALL_FAILED_DEPRECATED_SDK_VERSION: App package must target at least SDK version 24, but found 17\n")
	v := h.commandInstallWhys(ctx, req, fc, ds)[serial]
	if v.Code != "INSTALL_FAILED_DEPRECATED_SDK_VERSION" || !strings.Contains(v.Plain, "SDK 17") || v.Raw == "" {
		t.Fatalf("after the answer: %+v", v)
	}
	// And the Apps tab's box reads the same ask.
	diags, _ := d.LatestInstallDiagnoses(ctx, dev.ID)
	if x, ok := diags[pkg]; !ok || !x.Answered {
		t.Fatalf("latest diagnoses: %+v", diags)
	}
}
