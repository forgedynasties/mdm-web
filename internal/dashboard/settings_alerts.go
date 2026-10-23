package dashboard

import (
	"context"
	"fmt"
	"html"
	_ "image/gif"
	_ "image/jpeg"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"mdm/internal/alerts"
	"mdm/internal/db"

	"github.com/google/uuid"
)

func (h *Handler) SettingsSetAlertWebhook(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	// The form masks the stored URL (a bearer secret) and never re-sends it, so a blank
	// submission means "keep the existing value" rather than "clear it".
	if u := strings.TrimSpace(r.FormValue("alert_webhook_url")); u != "" {
		h.cfg.SetAlertWebhookURL(u)
	}
	h.audit(r, "alerts.webhook", "", "")
	h.hxDoneToast(w, r, h.settingsDest(r), "Settings saved", "success")
}

// hhmm renders minutes-past-midnight as a "HH:MM" string for <input type=time>.
func hhmm(min int) string {
	min = ((min % 1440) + 1440) % 1440
	return fmt.Sprintf("%02d:%02d", min/60, min%60)
}

// parseHHMM parses "HH:MM" into minutes-past-midnight, returning def on bad input.
func parseHHMM(s string, def int) int {
	var h, m int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d:%d", &h, &m); err != nil {
		return def
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return def
	}
	return h*60 + m
}

// serviceWindowView is one editable service window (fleet default or a group's).
type serviceWindowView struct {
	GroupID, GroupName                 string // GroupID "" = fleet default; otherwise a restaurant id
	Open, Close, NightOpen, NightClose string // HH:MM
	TZ                                 string
	HasOwn                             bool // restaurant has its own row (vs inheriting fleet)
	// Hours per weekday (H1, 23 Oct 2026). Days is always seven rows for the form (from
	// the week, else the one window every day); PerDay says the venue keeps a week.
	PerDay bool
	Days   []swDay
	Today  string // "11:00 – 21:00", "Closed today"; set for the venue page
	// AIO backend (B1): the hours follow it, or were set here while it has its own.
	Follows     bool
	BackendDays []swDay // the backend's week when the venue's hours are set here
}

// swDay is one weekday row: the form's values and the bar's position (% of the day).
type swDay struct {
	Name, Open, Close string
	Closed, Today     bool
	Left, Width       float64
}

func weekDays(wk *db.Week, open, close int) []swDay {
	names := []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
	out := make([]swDay, 7)
	for i := range out {
		d := db.DayHours{Open: open, Close: close}
		if wk != nil {
			d = wk[i]
		}
		sd := swDay{Name: names[i], Open: hhmm(d.Open), Close: hhmm(d.Close), Closed: d.Closed || d.Open == d.Close}
		if !sd.Closed {
			span := d.Close - d.Open
			if span <= 0 {
				span += 1440
			}
			sd.Left = float64(d.Open) * 100 / 1440
			sd.Width = math.Min(float64(span)*100/1440, 100-sd.Left)
		}
		out[i] = sd
	}
	return out
}

// dayLabel is a day's hours for people: "11:00 – 21:00", "All day", "Closed".
func (d swDay) Label() string {
	switch {
	case d.Closed:
		return "Closed"
	case d.Open == "00:00" && d.Close == "00:00":
		return "All day"
	}
	return d.Open + " – " + d.Close
}

func windowView(restaurantID, name string, w db.ServiceWindow, hasOwn bool) serviceWindowView {
	return serviceWindowView{
		GroupID: restaurantID, GroupName: name,
		Open: hhmm(w.OpenMin), Close: hhmm(w.CloseMin),
		NightOpen: hhmm(w.NightOpenMin), NightClose: hhmm(w.NightCloseMin),
		TZ: w.TZ, HasOwn: hasOwn,
		PerDay: w.Week != nil, Days: weekDays(w.Week, w.OpenMin, w.CloseMin),
	}
}

// buildServiceWindowViews returns the fleet default plus each restaurant's window for the
// Settings "Service hours" card.
func (h *Handler) buildServiceWindowViews(ctx context.Context) (serviceWindowView, []serviceWindowView) {
	fleet, _ := h.db.GetFleetServiceWindow(ctx)
	fleetView := windowView("", "", fleet, true)
	restaurants, _ := h.db.ListRestaurants(ctx)
	windows, _ := h.db.ListRestaurantServiceWindows(ctx) // one query, not one per restaurant
	var out []serviceWindowView
	for _, rest := range restaurants {
		if w, ok := windows[rest.ID]; ok {
			out = append(out, windowView(rest.ID.String(), rest.Name, w, true))
		} else {
			out = append(out, windowView(rest.ID.String(), rest.Name, fleet, false))
		}
	}
	return fleetView, out
}

// SettingsSetServiceWindow upserts a service window from the Settings form. An empty
// group_id sets the fleet default; "reset" on a restaurant deletes its row (inherit fleet).
// The form field is named group_id for back-compat but carries a restaurant id.
func (h *Handler) SettingsSetServiceWindow(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	gidStr := strings.TrimSpace(r.FormValue("group_id"))
	if gidStr != "" && r.FormValue("action") == "reset" {
		if gid, err := uuid.Parse(gidStr); err == nil {
			_ = h.db.DeleteServiceWindow(r.Context(), gid)
		}
		h.audit(r, "alerts.service_window", gidStr, "reset")
		h.settingsRedirect(w, r)
		return
	}
	sw := db.ServiceWindow{
		OpenMin:       parseHHMM(r.FormValue("open"), 420),
		CloseMin:      parseHHMM(r.FormValue("close"), 1380),
		NightOpenMin:  parseHHMM(r.FormValue("night_open"), 1410),
		NightCloseMin: parseHHMM(r.FormValue("night_close"), 360),
		TZ:            strings.TrimSpace(r.FormValue("timezone")),
	}
	if gidStr != "" {
		if gid, err := uuid.Parse(gidStr); err == nil {
			sw.RestaurantID = &gid
		} else {
			http.Error(w, "Invalid restaurant", http.StatusBadRequest)
			return
		}
	}
	if err := h.db.SetServiceWindow(r.Context(), sw); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "alerts.service_window", gidStr, "")
	h.settingsRedirect(w, r)
}

// SettingsSaveChannel creates a channel (no id) or updates one (id present).
func (h *Handler) SettingsSaveChannel(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	kind := r.FormValue("kind")
	if kind != "teams" {
		kind = "webhook"
	}
	// Unescape HTML entities (e.g. "&amp;" -> "&") before storing: a webhook URL pasted
	// from a rendered HTML source would otherwise persist a mangled query string
	// (sp/sv/sig become amp;sp/...), silently breaking all delivery.
	url := html.UnescapeString(strings.TrimSpace(r.FormValue("url")))
	c := db.AlertChannel{
		Name:          strings.TrimSpace(r.FormValue("name")),
		Kind:          kind,
		URL:           url,
		MinSeverity:   r.FormValue("min_severity"),
		Mode:          r.FormValue("mode"),
		ActiveWindow:  r.FormValue("active_window"),
		Enabled:       r.FormValue("enabled") == "on",
		NotifyResolve: r.FormValue("notify_resolve") == "on",
	}
	switch c.MinSeverity {
	case "info", "warning", "critical":
	default:
		c.MinSeverity = "warning"
	}
	if c.Mode != "digest" {
		c.Mode = "realtime"
	}
	if c.ActiveWindow != "service" && c.ActiveWindow != "overnight" {
		c.ActiveWindow = ""
	}
	// Per-type allowlist: keep only known types, de-duped. If every type is selected,
	// store nothing so the channel stays "all types" (back-compat and tidy).
	seen := map[string]bool{}
	for _, t := range r.Form["alert_types"] {
		if validAlertType(t) && !seen[t] {
			seen[t] = true
			c.AlertTypes = append(c.AlertTypes, t)
		}
	}
	if len(c.AlertTypes) >= countAlertTypes() {
		c.AlertTypes = nil
	}
	var err error
	if idStr := r.FormValue("id"); idStr != "" {
		if c.ID, err = uuid.Parse(idStr); err != nil {
			http.Error(w, "Invalid channel", http.StatusBadRequest)
			return
		}
		// The edit form masks the stored URL and never re-sends it, so a blank URL on
		// an update means "keep the existing one" rather than blanking delivery.
		if url == "" {
			if existing, e := h.db.GetAlertChannel(r.Context(), c.ID); e == nil {
				c.URL = existing.URL
			}
		}
		err = h.db.UpdateAlertChannel(r.Context(), c)
	} else {
		_, err = h.db.CreateAlertChannel(r.Context(), c)
	}
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "alerts.channel", c.Name, "")
	h.settingsRedirect(w, r)
}

// SettingsTestChannel sends a sample alert to one channel so the admin can confirm the
// webhook URL works before a real alert fires. Redirects back with a ?channel_test result.
func (h *Handler) SettingsTestChannel(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid channel", http.StatusBadRequest)
		return
	}
	c, err := h.db.GetAlertChannel(r.Context(), id)
	if err != nil {
		http.Error(w, "Channel not found", http.StatusNotFound)
		return
	}
	sample := db.AlertNotification{
		Type: "test", Severity: "info", Serial: "TEST-DEVICE",
		Summary: "Test alert from AIO MDM — this channel is wired up correctly.",
		EventAt: time.Now().UTC(),
	}
	result := "ok"
	if err := alerts.SendToChannel(r.Context(), c, sample); err != nil {
		log.Printf("[alert] test channel %q failed: %v", c.Name, err)
		result = "fail"
	}
	h.audit(r, "alerts.channel.test", c.Name, result)
	http.Redirect(w, r, "/alert-config?channel_test="+result+"#channels", http.StatusFound) // Alerts › Rules since S1
}

// SettingsDeleteChannel removes an alert channel.
func (h *Handler) SettingsDeleteChannel(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid channel", http.StatusBadRequest)
		return
	}
	if err := h.db.DeleteAlertChannel(r.Context(), id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "alerts.channel.delete", id.String(), "")
	h.settingsRedirect(w, r)
}
