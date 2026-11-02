package dashboard

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
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

// TestAccessDeniedPages: a page outside someone's access is a 403 page saying why (it was a
// bare 404 until 2 Nov 2026); only what doesn't exist is a 404; a fragment gets plain text.
func TestAccessDeniedPages(t *testing.T) {
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
	tmpl := template.Must(template.New("").Parse(`{{define "header"}}<main>{{end}}{{define "footer"}}</main>{{end}}`))
	tmpl = template.Must(tmpl.ParseFiles("../../templates/denied.html"))
	h := &Handler{db: d, cfg: cfg, hub: ws.NewHub(), loginFails: ratelimit.New(15 * time.Minute), tmpl: tmpl,
		store: sessions.NewCookieStore([]byte("0123456789abcdef0123456789abcdef"))}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	stamp := strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")
	sup, err := d.CreateUser(ctx, "sup-dn-"+stamp, "x", "operator", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteUser(ctx, sup.ID)
	rest, err := d.CreateRestaurant(ctx, db.Restaurant{Name: "DN venue " + stamp})
	if err != nil {
		t.Fatal(err)
	}
	b := 50
	labSerial, setupSerial := "DNLAB"+stamp, "DNSET"+stamp
	for _, s := range []string{labSerial, setupSerial} {
		if _, _, _, _, _, err := d.UpsertCheckin(ctx, s, "v2.1.099", &b, json.RawMessage(`{}`), false, "t7"); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.AssignDeviceToRestaurant(ctx, setupSerial, &rest.ID); err != nil {
		t.Fatal(err)
	}

	get := func(path string, hx bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		if hx {
			req.Header.Set("HX-Request", "true")
			req.Header.Del("Sec-Fetch-Mode")
		}
		req = req.WithContext(context.WithValue(req.Context(), ctxSessionKey{}, &db.Session{
			UserID: &sup.ID, Username: sup.Username, Role: "operator",
			LastSeen: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	for _, c := range []struct {
		name, path string
		code       int
		says       []string
	}{
		{"lab device", "/devices/" + labSerial, 403, []string{"have access</h1>", "in the Lab", "devices in service"}},
		{"device setting up", "/devices/" + setupSerial, 403, []string{"being set up at DN venue " + stamp}},
		{"venue with none of theirs", "/restaurants/" + rest.ID.String(), 403, []string{"Venue DN venue " + stamp, "None of its devices"}},
		{"missing venue", "/restaurants/" + uuid.NewString(), 404, []string{"Not found", "no venue at this address"}},
		{"super admin page", "/settings", 403, []string{"Only Super admins can open it."}},
	} {
		rec := get(c.path, false)
		body := rec.Body.String()
		if rec.Code != c.code {
			t.Errorf("%s: %d, want %d (%.200s)", c.name, rec.Code, c.code, body)
			continue
		}
		if rec.Header().Get("X-MDM-Page") != "error" {
			t.Errorf("%s: no X-MDM-Page header, so a boosted click would not swap it in", c.name)
		}
		for _, s := range c.says {
			if !strings.Contains(body, s) {
				t.Errorf("%s: page lacks %q:\n%.600s", c.name, s, body)
			}
		}
	}
	if rec := get("/restaurants/"+rest.ID.String()+"/members", true); rec.Code != 403 || strings.Contains(rec.Body.String(), "<main>") {
		t.Errorf("a fragment gets a plain 403: %d %.200s", rec.Code, rec.Body.String())
	}
}
