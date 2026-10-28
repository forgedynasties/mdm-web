package dashboard

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"

	"mdm/internal/db"
	prod "mdm/internal/product"
)

// The JSON API behind the AIO MDM Android app (A1 · Tabs, picked 29 Oct 2026), contract in
// docs/MOBILE_API.md. The bearer token is a normal dashboard session and every handler
// runs with it injected as the request's session, so roles, access rules and venue walls
// are the web's own: the reads reuse the pages' builders and the actions call the
// dashboard's handlers in-process.

const mobileDevicesPerPage = 50

// requireMobile resolves the bearer session and makes it the request's session.
func (h *Handler) requireMobile(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.appSession(r)
		if !ok || !roleKnown(s.Role) {
			appErr(w, http.StatusUnauthorized, "not signed in")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxSessionKey{}, s)))
	}
}

func (h *Handler) mobileSession(r *http.Request) *db.Session {
	s, _ := h.currentSession(r)
	return s
}

func (h *Handler) mobileLoginJSON(w http.ResponseWriter, r *http.Request, u *db.User, sid string, exp time.Time) {
	appJSON(w, http.StatusOK, map[string]any{
		"token": sid, "username": u.Username, "display_name": h.displayName(r, u.Username),
		"role": u.Role, "role_label": roleLabel(u.Role), "expires_at": exp,
	})
}

// MobileLogin: POST /api/v1/m/login. Any role may sign in; what they see follows it.
func (h *Handler) MobileLogin(w http.ResponseWriter, r *http.Request) {
	u, sid, exp, ok := h.passwordLogin(w, r, roleKnown, "this account has no role")
	if ok {
		h.mobileLoginJSON(w, r, u, sid, exp)
	}
}

func (h *Handler) MobileLogout(w http.ResponseWriter, r *http.Request) {
	if s := h.mobileSession(r); s != nil {
		_ = h.db.DeleteSession(r.Context(), s.ID)
	}
	appJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) MobileMe(w http.ResponseWriter, r *http.Request) {
	s := h.mobileSession(r)
	appJSON(w, http.StatusOK, map[string]any{
		"username": s.Username, "display_name": h.displayName(r, s.Username),
		"role": s.Role, "role_label": roleLabel(s.Role), "web_url": h.baseURL(r),
	})
}

// ── Microsoft sign-in for the app ───────────────────────────────────────────
//
// The browser leg is the web's (/auth/microsoft?app=1&challenge=…). Instead of a cookie
// session the callback hands the app a one-time code over aiomdm://signin, and the app
// trades it, with the PKCE verifier only it holds, for a token. A code caught by another
// app that registered the scheme is useless without the verifier.

const msAppCookie = "mdm-ms-app"

var msChallengeRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

type msAppCode struct {
	userID    uuid.UUID
	challenge string
	exp       time.Time
}

var msAppCodes sync.Map // code → msAppCode

func msAppRedirect(w http.ResponseWriter, q url.Values) {
	target := "aiomdm://signin?" + q.Encode()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// A page, not a 302: Chrome may refuse to leave for an app scheme without a tap.
	fmt.Fprintf(w, `<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1"><title>AIO MDM</title>
<body style="font-family:system-ui,sans-serif;display:grid;place-items:center;min-height:90vh;margin:0">
<p style="text-align:center">Back to the app…<br><br><a href="%[1]s" style="display:inline-block;padding:12px 22px;border-radius:99px;background:#cf4529;color:#fff;text-decoration:none;font-weight:600">Open AIO MDM</a></p>
<script>location.href=%[2]q</script></body>`, html.EscapeString(target), target)
}

// msAppFail answers a failed app sign-in back to the app; false when this is not one.
func msAppFail(w http.ResponseWriter, r *http.Request, msg string) bool {
	if c, err := r.Cookie(msAppCookie); err != nil || c.Value == "" {
		return false
	}
	http.SetCookie(w, &http.Cookie{Name: msAppCookie, Value: "", Path: "/", MaxAge: -1})
	msAppRedirect(w, url.Values{"error": {msg}})
	return true
}

// msAppHandOff finishes an app sign-in with a one-time code; false when this is not one.
func (h *Handler) msAppHandOff(w http.ResponseWriter, r *http.Request, u *db.User) bool {
	c, err := r.Cookie(msAppCookie)
	if err != nil || !msChallengeRE.MatchString(c.Value) {
		return false
	}
	http.SetCookie(w, &http.Cookie{Name: msAppCookie, Value: "", Path: "/", MaxAge: -1})
	code, err := newToken()
	if err != nil {
		msAppRedirect(w, url.Values{"error": {"Internal error, please try again."}})
		return true
	}
	now := time.Now()
	msAppCodes.Range(func(k, v any) bool {
		if now.After(v.(msAppCode).exp) {
			msAppCodes.Delete(k)
		}
		return true
	})
	msAppCodes.Store(code, msAppCode{userID: u.ID, challenge: c.Value, exp: now.Add(2 * time.Minute)})
	msAppRedirect(w, url.Values{"code": {code}})
	return true
}

// MobileLoginMicrosoft: POST /api/v1/m/login/microsoft {"code","verifier"}.
func (h *Handler) MobileLoginMicrosoft(w http.ResponseWriter, r *http.Request) {
	var body struct{ Code, Verifier string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil || body.Code == "" {
		appErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	v, ok := msAppCodes.LoadAndDelete(body.Code)
	if !ok || time.Now().After(v.(msAppCode).exp) {
		appErr(w, http.StatusUnauthorized, "sign-in expired, please try again")
		return
	}
	sum := sha256.Sum256([]byte(body.Verifier))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != v.(msAppCode).challenge {
		appErr(w, http.StatusUnauthorized, "sign-in could not be verified, please try again")
		return
	}
	u, err := h.db.GetUser(r.Context(), v.(msAppCode).userID)
	if err != nil {
		appErr(w, http.StatusUnauthorized, "account not found")
		return
	}
	sid, exp, err := h.openBearerSession(r, u)
	if err != nil {
		appErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.mobileLoginJSON(w, r, u, sid, exp)
}

// ── Shapes ──────────────────────────────────────────────────────────────────

type mRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type mHealth struct {
	Score int    `json:"score"`
	Label string `json:"label"` // healthy | watch | at_risk | none
}

type mVenue struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Initial string  `json:"initial"`
	IconURL *string `json:"icon_url"`
	Health  mHealth `json:"health"`
	OpenNow *bool   `json:"open_now"`
	Devices int     `json:"devices"`
	Online  int     `json:"online"`
	Alerts  int     `json:"alerts"`
	Hot     int     `json:"hot"`
}

type mDevice struct {
	Serial        string    `json:"serial"`
	Name          string    `json:"name"`
	Class         string    `json:"class"`
	ClassLabel    string    `json:"class_label"`
	Model         string    `json:"model"`
	Venue         *mRef     `json:"venue"`
	Online        bool      `json:"online"`
	LastSeen      time.Time `json:"last_seen"`
	BatteryPct    *int      `json:"battery_pct"`
	Charging      *bool     `json:"charging"`
	Locked        bool      `json:"locked"`
	Build         string    `json:"build"`
	ClientVersion string    `json:"client_version"`
}

type mAlert struct {
	ID       string    `json:"id"`
	Severity string    `json:"severity"`
	Title    string    `json:"title"`
	Summary  string    `json:"summary"`
	Device   *mRef     `json:"device"` // id = serial
	Venue    *mRef     `json:"venue"`
	FiredAt  time.Time `json:"fired_at"`
	State    string    `json:"state"` // open | acknowledged | snoozed
}

func healthLabel(class string, devices int) string {
	switch {
	case devices == 0:
		return "none"
	case class == "danger":
		return "at_risk"
	case class == "warn":
		return "watch"
	}
	return "healthy"
}

func nameInitial(name string) string {
	for _, r := range strings.TrimSpace(name) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return strings.ToUpper(string(r))
		}
	}
	return "?"
}

func (h *Handler) mVenueOf(c vcCard, icons map[uuid.UUID]int64) mVenue {
	v := mVenue{ID: c.ID, Name: c.Name, Initial: nameInitial(c.Name), Devices: c.Devices, Online: c.Online, Hot: c.HotNow,
		Health: mHealth{Score: c.H.Score, Label: healthLabel(c.Class, c.Devices)}}
	if !c.HasH || c.Devices == 0 {
		v.Health.Score = 0
	}
	v.Alerts = c.H.OpenCritical + c.H.OpenWarning
	if c.HasHours {
		open := c.Open
		v.OpenNow = &open
	}
	if id, err := uuid.Parse(c.ID); err == nil {
		if ver, ok := icons[id]; ok {
			u := fmt.Sprintf("/api/v1/m/venues/%s/icon?v=%d", c.ID, ver)
			v.IconURL = &u
		}
	}
	return v
}

// mobileVenues: the Venues page's cards (worst first), only venues this person may see.
func (h *Handler) mobileVenues(r *http.Request) []mVenue {
	rows, _, devices := h.collRowsDevs(r, "restaurants")
	page := buildVenuesV1(rows, h.venueExtras(r.Context(), devices), time.Now())
	acc := h.access(r)
	icons := h.db.VenueIconVersions(r.Context())
	out := make([]mVenue, 0, len(page.Cards))
	for _, c := range page.Cards {
		if id, err := uuid.Parse(c.ID); err != nil || !acc.venueInScope(id) {
			continue
		}
		out = append(out, h.mVenueOf(c, icons))
	}
	return out
}

type mExtra struct {
	Charging     *bool             `json:"charging"`
	BatteryOn    *bool             `json:"battery_present"`
	AgentVersion string            `json:"agent_version"`
	KioskActive  *bool             `json:"kiosk_active"`
	KioskSuspend bool              `json:"kiosk_suspended"`
	Wifi         json.RawMessage   `json:"wifi"`
	WifiRSSI     *int              `json:"wifi_rssi"`
	IP           string            `json:"ip_address"`
	Foreground   string            `json:"foreground_pkg"`
	Manufacturer string            `json:"manufacturer"`
	Model        string            `json:"model"`
	Lat          *float64          `json:"latitude"`
	Lon          *float64          `json:"longitude"`
	Accuracy     *float64          `json:"location_accuracy"`
	LostMode     struct{ On bool } `json:"lost_mode"`
	HostApp      *struct {
		Package string `json:"package"`
	} `json:"host_app"`
}

func parseMExtra(raw json.RawMessage) mExtra {
	var e mExtra
	_ = json.Unmarshal(raw, &e)
	return e
}

func (h *Handler) mDeviceOf(d db.Device, nick string, online bool) mDevice {
	e := parseMExtra(d.LatestExtra)
	m := mDevice{Serial: d.SerialNumber, Name: d.SerialNumber, Class: d.Class(), ClassLabel: d.ClassLabel(),
		Model: d.ProductLabel(), Online: online, LastSeen: d.LastSeenAt, Build: d.BuildID, ClientVersion: e.AgentVersion,
		Locked: d.KioskEnabled && !e.KioskSuspend}
	if nick != "" {
		m.Name = nick
	}
	if d.IsDPC() && e.Model != "" {
		m.Model = strings.TrimSpace(e.Manufacturer + " " + e.Model)
	}
	if e.KioskActive != nil {
		m.Locked = *e.KioskActive
	}
	if d.HasBattery() && (e.BatteryOn == nil || *e.BatteryOn) {
		pct := d.BatteryPct
		m.BatteryPct = &pct
		m.Charging = e.Charging
	}
	if d.RestaurantID != nil {
		m.Venue = &mRef{ID: d.RestaurantID.String(), Name: d.RestaurantName}
	}
	return m
}

func (h *Handler) mOnline(online map[uuid.UUID]bool, d db.Device) bool {
	return online[d.ID] || (!d.LastSeenAt.IsZero() && time.Since(d.LastSeenAt) <= time.Duration(h.cfg.CheckinInterval()*3)*time.Second)
}

func (h *Handler) mDevicesOf(ctx context.Context, devs []db.Device) []mDevice {
	ids := make([]uuid.UUID, len(devs))
	for i, d := range devs {
		ids[i] = d.ID
	}
	nicks, _ := h.db.GetNicknames(ctx, ids)
	online := h.onlineMap()
	out := make([]mDevice, 0, len(devs))
	for _, d := range devs {
		out = append(out, h.mDeviceOf(d, nicks[d.ID], h.mOnline(online, d)))
	}
	return out
}

func alertState(a db.Alert, now time.Time) string {
	if a.MutedUntil != nil && a.MutedUntil.After(now) {
		return "snoozed"
	}
	if a.Status == "acknowledged" {
		return "acknowledged"
	}
	return "open"
}

var tagRE = regexp.MustCompile(`<[^>]*>`)

func mAlertOf(a db.Alert, now time.Time) mAlert {
	ha := humanizeAlert(a)
	m := mAlert{ID: a.ID.String(), Severity: a.Severity, Title: ha.Headline,
		Summary: html.UnescapeString(tagRE.ReplaceAllString(string(ha.Sentence), "")), FiredAt: a.FiredAt, State: alertState(a, now)}
	if m.Title == "" {
		m.Title = alertTypeLabel(a.Type)
	}
	if a.Serial != "" {
		m.Device = &mRef{ID: a.Serial, Name: a.Serial}
	}
	if a.RestaurantID != "" {
		m.Venue = &mRef{ID: a.RestaurantID, Name: a.RestaurantName}
	}
	return m
}

// mobileAlerts: the active alerts this person may see, newest first.
func (h *Handler) mobileAlerts(r *http.Request) []db.Alert {
	active, err := h.db.ListActiveAlerts(r.Context(), 1000)
	if err != nil {
		return nil
	}
	active = h.access(r).keepVisibleAlerts(active)
	sort.SliceStable(active, func(i, j int) bool { return active[i].FiredAt.After(active[j].FiredAt) })
	return active
}

// ── Home ────────────────────────────────────────────────────────────────────

type mTarget struct {
	Kind string `json:"kind"` // device | venue | alert | alerts | web
	ID   string `json:"id,omitempty"`
	URL  string `json:"url,omitempty"`
}

type mTodo struct {
	Tone   string  `json:"tone"`
	Title  string  `json:"title"`
	Sub    string  `json:"sub"`
	Target mTarget `json:"target"`
}

var venueHrefRE = regexp.MustCompile(`^/restaurants/([0-9a-f-]{36})`)

// todoTarget turns a banner link into somewhere the app can go.
func (h *Handler) todoTarget(r *http.Request, href string) mTarget {
	p := strings.SplitN(href, "?", 2)[0]
	switch {
	case strings.HasPrefix(p, "/devices/") && !strings.Contains(strings.TrimPrefix(p, "/devices/"), "/"):
		return mTarget{Kind: "device", ID: strings.TrimPrefix(p, "/devices/")}
	case venueHrefRE.MatchString(p):
		return mTarget{Kind: "venue", ID: venueHrefRE.FindStringSubmatch(p)[1]}
	case p == "/alerts":
		return mTarget{Kind: "alerts"}
	}
	u := href
	if strings.HasPrefix(u, "/") {
		u = h.baseURL(r) + u
	}
	return mTarget{Kind: "web", URL: u}
}

func (h *Handler) MobileHome(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s := h.mobileSession(r)
	acc := h.access(r)
	venues := h.mobileVenues(r)
	alerts := h.mobileAlerts(r)
	now := time.Now()

	out := map[string]any{"name": h.displayName(r, s.Username)}
	if u, err := h.db.GetUserByUsername(ctx, s.Username); err == nil && strings.TrimSpace(u.FirstName) != "" {
		out["name"] = strings.TrimSpace(u.FirstName)
	}

	// Health: the average of the scored venues, as the Venues rail draws it.
	sum, n := 0, 0
	for _, v := range venues {
		if v.Devices > 0 && v.Health.Label != "none" {
			sum += v.Health.Score
			n++
		}
	}
	health := mHealth{Label: "none"}
	if n > 0 {
		health.Score = sum / n
		health.Label = "healthy"
		if health.Score < 50 {
			health.Label = "at_risk"
		} else if health.Score < 80 {
			health.Label = "watch"
		}
	}
	out["health"] = health

	// Stats over the devices this person sees.
	f := db.DeviceFilter{Connected: h.connectedSlice()}
	acc.applyFilter(&f)
	devs, _ := h.db.ListDevices(ctx, f, 0, 100000, "serial", "asc")
	online := h.onlineMap()
	nOn := 0
	for _, d := range devs {
		if h.mOnline(online, d) {
			nOn++
		}
	}
	nOpen := 0
	for _, a := range alerts {
		if alertState(a, now) == "open" {
			nOpen++
		}
	}
	pct := 0
	if len(devs) > 0 {
		pct = nOn * 100 / len(devs)
	}
	out["stats"] = map[string]int{"devices": len(devs), "online": nOn, "online_pct": pct, "open_alerts": nOpen}

	// To-do: the Overview's banner for those who see the whole fleet; otherwise the
	// critical alerts, then warnings, then venues at risk.
	var todo []mTodo
	total := 0
	if !acc.hidesDevices() && !roleIsCustomer(s.Role) {
		summary, _ := h.db.GetSummary(ctx, h.connectedSlice())
		groups, _ := h.db.GetRestaurantHealth(ctx, h.connectedSlice(), 7)
		crash, _ := h.db.GetFleetCrashStats(ctx, 4)
		inboxN := 0
		if fc, err := h.db.FleetCounts(ctx, acc.hidesDPC()); err == nil {
			inboxN = fc.Inbox
		}
		data := map[string]any{}
		h.overviewNewData(r, data, summary, groups, inboxN, crash)
		if b, ok := data["OvBanner"].(ovBanner); ok {
			tone := map[string]string{"bad": "critical", "warn": "warning"}[b.Tone]
			if tone == "" {
				tone = "info"
			}
			for _, t := range b.Todos {
				todo = append(todo, mTodo{Tone: tone, Title: t.Title, Sub: t.Sub, Target: h.todoTarget(r, t.Href)})
			}
			total = len(b.Todos) + b.More
		}
	} else {
		for _, sev := range []string{"critical", "warning"} {
			for _, a := range alerts {
				if a.Severity != sev || alertState(a, now) != "open" {
					continue
				}
				total++
				if len(todo) < 3 {
					m := mAlertOf(a, now)
					sub := m.Summary
					if m.Venue != nil && m.Venue.Name != "" {
						sub = m.Venue.Name + " · " + sub
					}
					todo = append(todo, mTodo{Tone: sev, Title: m.Title, Sub: sub, Target: mTarget{Kind: "alert", ID: m.ID}})
				}
			}
		}
		for _, v := range venues {
			if v.Health.Label == "at_risk" {
				total++
				if len(todo) < 3 {
					todo = append(todo, mTodo{Tone: "warning", Title: v.Name + " needs a look",
						Sub: fmt.Sprintf("%d of %d online", v.Online, v.Devices), Target: mTarget{Kind: "venue", ID: v.ID}})
				}
			}
		}
	}
	if todo == nil {
		todo = []mTodo{}
	}
	out["todo"], out["todo_total"] = todo, total
	if len(venues) > 8 {
		venues = venues[:8]
	}
	out["venues"] = venues
	appJSON(w, http.StatusOK, out)
}

// ── Devices ─────────────────────────────────────────────────────────────────

func (h *Handler) MobileDevices(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	f := db.DeviceFilter{Search: strings.TrimSpace(q.Get("q")), Connected: h.connectedSlice()}
	if o := q.Get("online"); o == "online" || o == "offline" {
		f.Online = o
	}
	if v, err := uuid.Parse(q.Get("venue")); err == nil {
		f.RestaurantID = v
	}
	h.access(r).applyFilter(&f)
	devs, err := h.db.ListDevices(ctx, f, 0, 100000, "serial", "asc")
	if err != nil {
		appErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Type chips count what the other filters leave; the type filter applies after.
	counts := map[string]int{}
	class := q.Get("class")
	kept := devs[:0:0]
	for _, d := range devs {
		c := d.Class()
		counts[c]++
		if class == "" || c == class {
			kept = append(kept, d)
		}
	}
	classes := []map[string]any{}
	for _, c := range prod.Classes() {
		if counts[c] > 0 {
			classes = append(classes, map[string]any{"key": c, "label": prod.ClassLabel(c), "count": counts[c]})
		}
	}
	sort.SliceStable(classes, func(i, j int) bool { return classes[i]["count"].(int) > classes[j]["count"].(int) })
	// Online first, then by serial.
	online := h.onlineMap()
	sort.SliceStable(kept, func(i, j int) bool {
		a, b := h.mOnline(online, kept[i]), h.mOnline(online, kept[j])
		return a && !b
	})
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	from := (page - 1) * mobileDevicesPerPage
	if from > len(kept) {
		from = len(kept)
	}
	to := min(from+mobileDevicesPerPage, len(kept))
	appJSON(w, http.StatusOK, map[string]any{
		"devices": h.mDevicesOf(ctx, kept[from:to]), "total": len(kept), "page": page,
		"per_page": mobileDevicesPerPage, "classes": classes,
	})
}

// mobileDevice loads a device the person may see, or answers 404.
func (h *Handler) mobileDevice(w http.ResponseWriter, r *http.Request) (*db.Device, bool) {
	d, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
	if err != nil || d == nil || !h.access(r).visible(d.ID) {
		appErr(w, http.StatusNotFound, "device not found")
		return nil, false
	}
	return d, true
}

type mRecent struct {
	At    time.Time `json:"at"`
	Title string    `json:"title"`
	Sub   string    `json:"sub"`
	Tone  string    `json:"tone"`
}

func cmdTone(status string) string {
	switch status {
	case "completed", "installed":
		return "ok"
	case "failed", "expired":
		return "critical"
	case "cancelled":
		return "info"
	}
	return "warning"
}

// mobileRecent is the device page's "Recent on this device": commands and changes, newest
// first, no control probes.
func (h *Handler) mobileRecent(r *http.Request, d *db.Device, online bool) []mRecent {
	ctx := r.Context()
	role := h.role(r)
	var out []mRecent
	if cmds, err := h.db.GetDeviceCommands(ctx, d.ID, h.cfg.CommandExpiry()); err == nil {
		redactDeviceCommandURLs(role, cmds)
		for _, c := range filterShellDeviceCommands(role, cmds) {
			if timelineIsProbe(c) {
				continue
			}
			title, hint := cmdTitleOf(shapeOf(c))
			status := c.Status
			if !online && (status == "delivered" || status == "pending") {
				status = "queued"
			} else if status == "delivered" {
				status = "pending"
			}
			sub := status
			if hint != "" && c.Type != "shell" {
				sub = hint + " · " + sub
			}
			if c.CreatedBy != "" {
				sub += " · " + h.displayName(r, c.CreatedBy)
			}
			out = append(out, mRecent{At: c.CreatedAt, Title: title, Sub: sub, Tone: cmdTone(c.Status)})
		}
	}
	entries, _ := h.db.ListAuditForDevice(ctx, d.ID, 50)
	for _, e := range h.deviceActivityItems(ctx, role, entries, 20) {
		out = append(out, mRecent{At: e.At, Title: e.Actor + " " + e.Did, Tone: "info"})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	if len(out) > timelineRecent {
		out = out[:timelineRecent]
	}
	if out == nil {
		out = []mRecent{}
	}
	return out
}

// mobileLockApp is the Lock button's view of the device (nil: its role has no app).
func (h *Handler) mobileLockApp(ctx context.Context, d *db.Device) *LockApp {
	cfg, err := h.db.GetOrCreateDeviceConfig(ctx, d.ID)
	if err != nil {
		return nil
	}
	pkgs, err := h.db.GetDevicePackages(ctx, d.ID)
	if err != nil {
		return nil
	}
	return lockAppFor(*d, h.db.RoleAppBase(ctx, d.Class()), pkgs, cfg, h.cfg.KioskAppAllowed)
}

func (h *Handler) MobileDevice(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d, ok := h.mobileDevice(w, r)
	if !ok {
		return
	}
	acc := h.access(r)
	role := h.role(r)
	nicks, _ := h.db.GetNicknames(ctx, []uuid.UUID{d.ID})
	online := h.mOnline(h.onlineMap(), *d)
	base := h.mDeviceOf(*d, nicks[d.ID], online)
	e := parseMExtra(d.LatestExtra)

	dev := map[string]any{}
	if b, err := json.Marshal(base); err == nil {
		_ = json.Unmarshal(b, &dev)
	}
	dev["temp_c"], dev["temp_src"] = nil, nil
	if t, src, ok := deviceTempC(d.LatestExtra); ok {
		dev["temp_c"], dev["temp_src"] = float64(int(t*10+0.5))/10, src
	}
	dev["wifi"] = nil
	var ssid string
	if json.Unmarshal(e.Wifi, &ssid) != nil {
		var wo struct{ SSID string }
		_ = json.Unmarshal(e.Wifi, &wo)
		ssid = wo.SSID
	}
	ssid = strings.Trim(ssid, `"`)
	if ssid != "" && ssid != "<unknown ssid>" {
		dev["wifi"] = map[string]any{"ssid": ssid, "rssi": e.WifiRSSI}
	}
	roleApp := h.db.RoleAppBase(ctx, d.Class())
	nameOf := func(pkg string) string {
		if roleApp != "" && (pkg == roleApp || strings.HasPrefix(pkg, roleApp+".")) {
			env, _ := hostAppEnv(strings.TrimPrefix(strings.TrimPrefix(pkg, roleApp), "."))
			return strings.TrimSpace(hostAppName(roleApp) + " " + env)
		}
		return pkg
	}
	dev["on_screen"] = nil
	if fg := e.Foreground; fg != "" {
		dev["on_screen"] = nameOf(fg)
	} else if e.HostApp != nil && e.HostApp.Package != "" {
		dev["on_screen"] = nameOf(e.HostApp.Package)
	}
	dev["locked_to"] = nil
	if base.Locked && d.KioskPackage != "" {
		dev["locked_to"] = d.KioskPackage
	}
	dev["lost"] = e.LostMode.On
	dev["location"] = nil
	if e.Lat != nil && e.Lon != nil && (*e.Lat != 0 || *e.Lon != 0) {
		dev["location"] = map[string]any{"lat": *e.Lat, "lon": *e.Lon, "accuracy_m": e.Accuracy}
	}
	dev["ip"] = nil
	if e.IP != "" {
		dev["ip"] = e.IP
	}

	needs := []map[string]string{}
	for _, n := range deviceNeedsOf(h.securityFor(ctx, d), d, d.HasBattery(), false) {
		tone := "warning"
		if n.Tone == "bad" {
			tone = "critical"
		}
		needs = append(needs, map[string]string{"tone": tone, "title": n.Title, "why": n.Why})
	}
	now := time.Now()
	alerts := []mAlert{}
	for _, a := range acc.keepVisibleAlerts(h.listDeviceAlerts(ctx, d.ID)) {
		alerts = append(alerts, mAlertOf(a, now))
	}

	can := func(action string) bool { return acc.canDevice(action, d.ID) }
	la := h.mobileLockApp(ctx, d)
	lockOK := roleCanSupport(role) && can("kiosk")
	actions := map[string]bool{
		"reboot":     h.commandTypeAllowed(role, "reboot") && can("reboot") && d.Supports("reboot"),
		"screenshot": h.commandTypeAllowed(role, "screenshot") && can("screenshot") && d.Supports("screenshot"),
		"lock":       lockOK && !base.Locked && la != nil && len(la.Variants) > 0,
		"unlock":     lockOK && (base.Locked || d.KioskEnabled),
		"locate":     dev["location"] != nil,
	}
	out := map[string]any{
		"device": dev, "needs": needs, "alerts": alerts, "recent": h.mobileRecent(r, d, online),
		"actions": actions, "web_url": h.baseURL(r) + "/devices/" + url.PathEscape(d.SerialNumber),
	}
	if la != nil && len(la.Variants) > 1 {
		var choices []map[string]any
		for _, v := range la.Variants {
			choices = append(choices, map[string]any{"package": v.Package, "label": strings.TrimSpace(la.Name + " " + v.Env), "on_screen": v.OnScreen})
		}
		out["lock_choices"] = choices
	}
	appJSON(w, http.StatusOK, out)
}

func (h *Handler) listDeviceAlerts(ctx context.Context, id uuid.UUID) []db.Alert {
	a, _ := h.db.ListDeviceActiveAlerts(ctx, id, 10)
	return a
}

// mobileCall runs a dashboard handler in-process with this request's session and a form
// body, and returns what it answered.
func mobileCall(r *http.Request, method, path string, form url.Values, pathVals map[string]string, handler http.HandlerFunc) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(form.Encode())).WithContext(r.Context())
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = r.RemoteAddr
	req.Host = r.Host
	for k, v := range pathVals {
		req.SetPathValue(k, v)
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// mobileCallErr turns a failed in-process answer into a message for a person.
func mobileCallErr(rec *httptest.ResponseRecorder) string {
	body := strings.TrimSpace(rec.Body.String())
	var j struct{ Error string }
	if json.Unmarshal([]byte(body), &j) == nil && j.Error != "" {
		return j.Error
	}
	if body == "" || strings.HasPrefix(body, "<") || len(body) > 200 {
		return http.StatusText(rec.Code)
	}
	return body
}

// MobileDeviceAction: POST /api/v1/m/devices/{serial}/actions {"action","reason"?,"package"?}.
func (h *Handler) MobileDeviceAction(w http.ResponseWriter, r *http.Request) {
	d, ok := h.mobileDevice(w, r)
	if !ok {
		return
	}
	var body struct{ Action, Reason, Package string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		appErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	serial := d.SerialNumber
	sp := map[string]string{"serial": serial}
	switch body.Action {
	case "reboot", "screenshot":
		form := url.Values{"type": {body.Action}, "target_type": {"devices"}, "target_serials": {serial}}
		if body.Reason != "" {
			form.Set("reason", body.Reason)
		}
		rec := mobileCall(r, "POST", "/commands", form, nil, h.CommandCreate)
		loc := rec.Header().Get("Location")
		if rec.Code >= 400 || !strings.HasPrefix(loc, "/commands/") {
			code := rec.Code
			if code < 400 {
				code = http.StatusConflict
			}
			msg := mobileCallErr(rec)
			if strings.HasPrefix(loc, "/commands?flash=") {
				if u, err := url.Parse(loc); err == nil {
					msg = u.Query().Get("flash")
				}
			}
			appErr(w, code, msg)
			return
		}
		id := strings.SplitN(strings.TrimPrefix(loc, "/commands/"), "?", 2)[0]
		msg := map[string]string{"reboot": "Rebooting", "screenshot": "Taking a screenshot"}[body.Action]
		appJSON(w, http.StatusOK, map[string]any{"ok": true, "command_id": id, "message": msg})
	case "lock", "unlock":
		if !roleCanSupport(h.role(r)) {
			appErr(w, http.StatusForbidden, "your role cannot lock devices")
			return
		}
		la := h.mobileLockApp(r.Context(), d)
		var rec *httptest.ResponseRecorder
		switch {
		case body.Action == "lock":
			if la == nil || len(la.Variants) == 0 {
				appErr(w, http.StatusBadRequest, "This device has no host app to lock to")
				return
			}
			pkg := la.Variants[0].Package
			if body.Package != "" {
				pkg = body.Package
			}
			rec = mobileCall(r, "POST", "/devices/"+serial+"/lock", url.Values{"on": {"1"}, "package": {pkg}}, sp, h.deviceRoute("kiosk", h.DeviceLockHost))
		case la != nil:
			rec = mobileCall(r, "POST", "/devices/"+serial+"/lock", url.Values{"on": {"0"}}, sp, h.deviceRoute("kiosk", h.DeviceLockHost))
		default:
			rec = mobileCall(r, "POST", "/devices/"+serial+"/kiosk", url.Values{"kiosk_enabled": {"0"}}, sp, h.deviceRoute("kiosk", h.DeviceKioskUpdate))
		}
		if rec.Code >= 400 {
			appErr(w, rec.Code, mobileCallErr(rec))
			return
		}
		var res struct {
			Delivered bool
			Sub       string
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &res)
		msg := "Unlocked"
		if body.Action == "lock" {
			msg = "Locked"
			if res.Sub != "" {
				msg += " to " + res.Sub
			}
		}
		if !res.Delivered && !h.hub.IsConnected(d.ID) {
			msg += " · applies when it is back online"
		}
		appJSON(w, http.StatusOK, map[string]any{"ok": true, "command_id": nil, "message": msg})
	default:
		appErr(w, http.StatusBadRequest, "unknown action")
	}
}

// MobileCommand: GET /api/v1/m/commands/{id} — this person's view of one device's run.
func (h *Handler) MobileCommand(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		appErr(w, http.StatusNotFound, "command not found")
		return
	}
	cmd, err := h.db.GetCommand(r.Context(), id)
	if err != nil || h.commandHiddenFrom(r, *cmd) {
		appErr(w, http.StatusNotFound, "command not found")
		return
	}
	ds, err := h.db.GetCommandDeliveries(r.Context(), id, h.cfg.CommandExpiry())
	if err != nil {
		appErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	ds = h.keepVisibleDeliveries(r, ds)
	if len(ds) == 0 {
		appErr(w, http.StatusNotFound, "command not found")
		return
	}
	dl := ds[0]
	out := map[string]any{"status": dl.Status, "output": nil, "screenshot_url": nil}
	if cmd.Type == "screenshot" {
		if dl.Status == "completed" && dl.Output != "" {
			out["screenshot_url"] = "/api/v1/m/commands/" + id.String() + "/screenshot/" + url.PathEscape(dl.SerialNumber)
		} else if dl.Status == "failed" {
			out["output"] = dl.Output
		}
	} else if cmd.Type != "shell" {
		out["output"] = dl.Output
	}
	appJSON(w, http.StatusOK, out)
}

// ── Alerts ──────────────────────────────────────────────────────────────────

func (h *Handler) MobileAlerts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state, sev := q.Get("state"), q.Get("severity")
	if state == "" {
		state = "open"
	}
	var since time.Time
	if s := q.Get("since"); s != "" {
		since, _ = time.Parse(time.RFC3339, s)
	}
	now := time.Now()
	counts := map[string]int{"open": 0, "acknowledged": 0, "snoozed": 0}
	out := []mAlert{}
	for _, a := range h.mobileAlerts(r) {
		st := alertState(a, now)
		counts[st]++
		if st != state || (sev != "" && a.Severity != sev) || (!since.IsZero() && !a.FiredAt.After(since)) {
			continue
		}
		out = append(out, mAlertOf(a, now))
	}
	appJSON(w, http.StatusOK, map[string]any{"alerts": out, "counts": counts})
}

func (h *Handler) mobileAlertOp(w http.ResponseWriter, r *http.Request, op, value string) {
	if _, err := uuid.Parse(r.PathValue("id")); err != nil {
		appErr(w, http.StatusNotFound, "alert not found")
		return
	}
	if !roleCanSupport(h.role(r)) {
		appErr(w, http.StatusForbidden, "your role cannot handle alerts")
		return
	}
	form := url.Values{"op": {op}, "ids": {r.PathValue("id")}, "value": {value}}
	rec := mobileCall(r, "POST", "/alerts/problem", form, nil, h.AlertProblemAction)
	if rec.Code >= 400 {
		appErr(w, rec.Code, mobileCallErr(rec))
		return
	}
	appJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) MobileAlertAck(w http.ResponseWriter, r *http.Request) {
	h.mobileAlertOp(w, r, "ack", "")
}

func (h *Handler) MobileAlertSnooze(w http.ResponseWriter, r *http.Request) {
	var body struct{ Hours int }
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body)
	if body.Hours != 1 && body.Hours != 4 && body.Hours != 24 {
		appErr(w, http.StatusBadRequest, "hours must be 1, 4 or 24")
		return
	}
	h.mobileAlertOp(w, r, "snooze", strconv.Itoa(body.Hours))
}

// ── Venues ──────────────────────────────────────────────────────────────────

func (h *Handler) MobileVenues(w http.ResponseWriter, r *http.Request) {
	appJSON(w, http.StatusOK, map[string]any{"venues": h.mobileVenues(r)})
}

func (h *Handler) mobileVenueID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil || !h.access(r).venueInScope(id) {
		appErr(w, http.StatusNotFound, "venue not found")
		return uuid.Nil, false
	}
	return id, true
}

func (h *Handler) MobileVenue(w http.ResponseWriter, r *http.Request) {
	id, ok := h.mobileVenueID(w, r)
	if !ok {
		return
	}
	rest, err := h.db.GetRestaurant(r.Context(), id)
	if err != nil {
		appErr(w, http.StatusNotFound, "venue not found")
		return
	}
	var venue map[string]any
	for _, v := range h.mobileVenues(r) {
		if v.ID == id.String() {
			b, _ := json.Marshal(v)
			_ = json.Unmarshal(b, &venue)
		}
	}
	if venue == nil {
		appErr(w, http.StatusNotFound, "venue not found")
		return
	}
	venue["address"], venue["timezone"] = rest.Address, rest.Timezone
	f := db.DeviceFilter{RestaurantID: id, Connected: h.connectedSlice()}
	h.access(r).applyFilter(&f)
	devs, _ := h.db.ListDevices(r.Context(), f, 0, 2000, "serial", "asc")
	online := h.onlineMap()
	sort.SliceStable(devs, func(i, j int) bool { return h.mOnline(online, devs[i]) && !h.mOnline(online, devs[j]) })
	appJSON(w, http.StatusOK, map[string]any{"venue": venue, "devices": h.mDevicesOf(r.Context(), devs)})
}

func (h *Handler) MobileVenueIcon(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.mobileVenueID(w, r); ok {
		h.RestaurantIconImage(w, r)
	}
}

// MobileScreenshot serves a screenshot result to the app (the web handler's rules).
func (h *Handler) MobileScreenshot(w http.ResponseWriter, r *http.Request) {
	h.CommandScreenshot(w, r)
}

func (h *Handler) registerMobileRoutes(mux *http.ServeMux) {
	m := h.requireMobile
	mux.HandleFunc("POST /api/v1/m/login", h.MobileLogin)
	mux.HandleFunc("POST /api/v1/m/login/microsoft", h.MobileLoginMicrosoft)
	mux.HandleFunc("POST /api/v1/m/logout", m(h.MobileLogout))
	mux.HandleFunc("GET /api/v1/m/me", m(h.MobileMe))
	mux.HandleFunc("GET /api/v1/m/home", m(h.MobileHome))
	mux.HandleFunc("GET /api/v1/m/devices", m(h.MobileDevices))
	mux.HandleFunc("GET /api/v1/m/devices/{serial}", m(h.MobileDevice))
	mux.HandleFunc("POST /api/v1/m/devices/{serial}/actions", m(h.MobileDeviceAction))
	mux.HandleFunc("GET /api/v1/m/commands/{id}", m(h.MobileCommand))
	mux.HandleFunc("GET /api/v1/m/commands/{id}/screenshot/{serial}", m(h.MobileScreenshot))
	mux.HandleFunc("GET /api/v1/m/alerts", m(h.MobileAlerts))
	mux.HandleFunc("POST /api/v1/m/alerts/{id}/ack", m(h.MobileAlertAck))
	mux.HandleFunc("POST /api/v1/m/alerts/{id}/snooze", m(h.MobileAlertSnooze))
	mux.HandleFunc("GET /api/v1/m/venues", m(h.MobileVenues))
	mux.HandleFunc("GET /api/v1/m/venues/{id}", m(h.MobileVenue))
	mux.HandleFunc("GET /api/v1/m/venues/{id}/icon", m(h.MobileVenueIcon))
}
