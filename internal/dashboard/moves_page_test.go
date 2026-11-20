package dashboard

import (
	"bytes"
	"context"
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
	"mdm/internal/otagate"
	"mdm/internal/ratelimit"
	"mdm/internal/ws"
)

func TestBuildMovesView(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	flights, hidden := uuid.New(), uuid.New()
	ago := func(d time.Duration) *time.Time { x := now.Add(-d); return &x }
	arrived := now.Add(-time.Hour)
	moves := []db.DeviceMove{
		{Serial: "A1", State: db.MoveArrived, Kind: "dpc", VenueID: &flights, Venue: "Flights Vegas", ArrivedAt: &arrived, LastSeenAt: ago(time.Minute)},
		{Serial: "A2", State: db.MovePending, Kind: "dpc", VenueID: &flights, Venue: "Flights Vegas", LastSeenAt: ago(time.Minute), Note: "waiting for MDM client 0.15.1 → 0.15.2", ClientVersion: "0.15.1"},
		{Serial: "A3", State: db.MovePending, Kind: "firmware", VenueID: &hidden, Venue: "Old Place", VenueHidden: true, LastSeenAt: ago(30 * 24 * time.Hour)},
		{Serial: "A4", State: db.MovePending, Kind: "dpc", LastSeenAt: ago(time.Hour), Note: "client too old and cannot update (0.13.3, needs 0.13.4)"},
		{Serial: "A5", State: db.MoveCameBack, Kind: "dpc", LastSeenAt: ago(time.Minute), Note: "came back"},
		{Serial: "A6", State: db.MoveFailed, Kind: "dpc", VenueID: &flights, Venue: "Flights Vegas", Note: "refused"},
		{Serial: "A7", State: db.MoveSent, Kind: "dpc", LastSeenAt: ago(time.Minute)},
	}
	st := func(m db.DeviceMove) db.ClientStanding {
		if m.Serial == "A2" {
			return db.ClientStanding{State: "behind", Have: "0.15.1", Want: "0.15.2"}
		}
		return db.ClientStanding{State: "current"}
	}
	v := buildMovesView(moves, st, now)
	if v.Total != 7 || v.Arrived != 1 {
		t.Fatalf("total/arrived = %d/%d", v.Total, v.Arrived)
	}
	for k, want := range map[string]int{"pending": 2, "waiting_client": 1, "sent": 1, "arrived": 1, "came_back": 1, "failed": 1, "attention": 4} {
		if v.Counts[k] != want {
			t.Errorf("counts[%s] = %d, want %d", k, v.Counts[k], want)
		}
	}
	// Attention: came back, failed, too old, dark, in that order.
	var kinds []string
	for _, a := range v.Attention {
		kinds = append(kinds, a.Kind)
	}
	if strings.Join(kinds, ",") != "came_back,failed,tooold,dark" {
		t.Errorf("attention = %v", kinds)
	}
	// Places: To place first (A4, A5, A7), then the venue with the most left (Flights: 2 of 3 left).
	if len(v.Places) != 3 || !v.Places[0].ToPlace || v.Places[0].Total != 3 || v.Places[1].Name != "Flights Vegas" ||
		v.Places[1].Total != 3 || v.Places[1].Arrived != 1 || !v.Places[2].Hidden {
		t.Errorf("places = %+v", v.Places)
	}
	byS := map[string]moveRow{}
	for _, r := range v.Moves {
		byS[r.Serial] = r
	}
	if r := byS["A2"]; r.Bucket != "waiting_client" || r.Standing != "behind" || r.Want != "0.15.2" || !r.Online {
		t.Errorf("A2 = %+v", r)
	}
	if r := byS["A3"]; r.DarkDays != 30 || r.Online || r.Next != "waiting for it to come online" || r.Kind != "fw" {
		t.Errorf("A3 = %+v", r)
	}
	if r := byS["A4"]; r.Standing != "tooold" || r.Steps[2].State != "bad" {
		t.Errorf("A4 = %+v", r)
	}
	if v.OnlinePend != 1 {
		t.Errorf("online pending = %d", v.OnlinePend)
	}
	last := byS["A1"].Steps
	if last[len(last)-1].Title != "Arrived on prod" || last[len(last)-1].State != "done" {
		t.Errorf("A1 steps = %+v", last)
	}
}

func TestMovesTemplateRenders(t *testing.T) {
	tmpl := hubTmpl(t, "moves.html")
	v := buildMovesView(nil, nil, time.Now())
	var buf bytes.Buffer
	data := map[string]any{"V": v, "JSON": template.JS(`{"total":0}`)}
	if err := tmpl.ExecuteTemplate(&buf, "moves.html", data); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`id="mv-data"`, `/static/css/moves.css`, `/static/js/moves.js`, `id="mv-rail"`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

// /moves exists only where moves do, for the super admin alone.
func TestMovesPageAccess(t *testing.T) {
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
	tmpl := template.Must(template.New("").Funcs(template.FuncMap{"assetVer": func() string { return "t" }, "asset": func(p string) string { return "t" }}).
		Parse(`{{define "header"}}<main>{{end}}{{define "footer"}}</main>{{end}}`))
	tmpl = template.Must(tmpl.ParseFiles("../../templates/moves.html", "../../templates/denied.html"))
	h := &Handler{db: d, cfg: cfg, hub: ws.NewHub(), loginFails: ratelimit.New(15 * time.Minute), tmpl: tmpl, otaGate: otagate.New(d, cfg),
		store: sessions.NewCookieStore([]byte("0123456789abcdef0123456789abcdef"))}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	get := func(path, role string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		req = req.WithContext(context.WithValue(req.Context(), ctxSessionKey{}, &db.Session{
			Username: "t-" + role, Role: role, LastSeen: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	t.Setenv("PUBLIC_ORIGIN", "http://127.0.0.1:1")
	t.Setenv("ALLOW_DEVICE_MOVE", "")
	if c := get("/moves", "admin").Code; c != 404 {
		t.Errorf("admin where nothing moves: %d, want 404", c)
	}
	if c := get("/moves/data", "admin").Code; c != 404 {
		t.Errorf("data where nothing moves: %d, want 404", c)
	}
	t.Setenv("ALLOW_DEVICE_MOVE", "1")
	t.Setenv("MOVE_TARGET_URL", "https://mdm.example.invalid")
	if c := get("/moves", "admin"); c.Code != 200 || !strings.Contains(c.Body.String(), `id="mv-data"`) {
		t.Errorf("admin: %d", c.Code)
	}
	if c := get("/moves/data", "admin"); c.Code != 200 || !strings.Contains(c.Body.String(), `"moves"`) {
		t.Errorf("admin data: %d %s", c.Code, c.Body.String())
	}
	for _, role := range []string{"operator", "fleet_manager", "viewer"} {
		if c := get("/moves", role).Code; c != 403 {
			t.Errorf("%s: %d, want 403", role, c)
		}
		if c := get("/moves/data", role).Code; c != 403 {
			t.Errorf("%s data: %d, want 403", role, c)
		}
	}
}
