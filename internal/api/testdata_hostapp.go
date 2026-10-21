package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Host app on screen, for the testing team (QA key, 21 Oct 2026; user: "add an endpoint
// so they can see when host app is on screen and when its not"). Replays the host_app_fg
// and screen_on transitions (db.HostAppScreenEvents) into spans:
//
//	on_screen  · app_in_foreground       the host app's variant in use is in front, screen on
//	off_screen · app_not_in_foreground   something else is in front (launcher, settings, …)
//	off_screen · screen_off              the app is in front but the screen is off
//	unknown    · no_report               no host app report kept yet for that time
//	unknown    · device_offline          after the device's last check-in, while it is offline
//
// Same scope as the other testdata routes: firmware devices, ?group= / ?restaurant=.

type qaScreenSpan struct {
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	State   string    `json:"state"`
	Why     string    `json:"why"`
	Seconds int64     `json:"seconds"`
}

type qaScreenTotals struct {
	OnScreenS   int64 `json:"on_screen_s"`
	OffScreenS  int64 `json:"off_screen_s"`
	UnknownS    int64 `json:"unknown_s"`
	OnScreenPct *int  `json:"on_screen_pct"` // of the time the state is known; null when none is
}

type qaScreenNow struct {
	State string    `json:"state"`
	Why   string    `json:"why"`
	Since time.Time `json:"since"` // when this state began, or the window start if earlier
}

type qaScreenEvent struct {
	T   time.Time `json:"t"`
	Key string    `json:"key"` // host_app_fg | screen_on
	To  bool      `json:"to"`
}

func screenState(fg, scr string) (string, string) {
	switch {
	case fg == "":
		return "unknown", "no_report"
	case fg != "true":
		return "off_screen", "app_not_in_foreground"
	case scr == "false":
		return "off_screen", "screen_off"
	}
	return "on_screen", "app_in_foreground"
}

// hostAppSpans replays a device's transitions over [from, to). offlineFrom is when the
// device went quiet (zero when it is online): from then on the state is unknown.
func hostAppSpans(initial map[string]string, evs []db.ScreenStateEvent, from, to, offlineFrom time.Time) ([]qaScreenSpan, qaScreenTotals) {
	cur := map[string]string{"host_app_fg": initial["host_app_fg"], "screen_on": initial["screen_on"]}
	var spans []qaScreenSpan
	push := func(a, b time.Time, state, why string) {
		if !b.After(a) {
			return
		}
		if n := len(spans); n > 0 && spans[n-1].State == state && spans[n-1].Why == why && spans[n-1].To.Equal(a) {
			spans[n-1].To = b
			return
		}
		spans = append(spans, qaScreenSpan{From: a, To: b, State: state, Why: why})
	}
	end := to
	if !offlineFrom.IsZero() && offlineFrom.Before(to) {
		end = offlineFrom
		if end.Before(from) {
			end = from
		}
	}
	at := from
	for _, e := range evs {
		if e.At.Before(from) || !e.At.Before(to) {
			continue
		}
		t := e.At
		if t.After(end) {
			t = end
		}
		st, why := screenState(cur["host_app_fg"], cur["screen_on"])
		push(at, t, st, why)
		if t.After(at) {
			at = t
		}
		cur[e.Key] = e.To
	}
	st, why := screenState(cur["host_app_fg"], cur["screen_on"])
	push(at, end, st, why)
	push(end, to, "unknown", "device_offline")

	var tot qaScreenTotals
	for i := range spans {
		s := int64(spans[i].To.Sub(spans[i].From) / time.Second)
		spans[i].Seconds = s
		switch spans[i].State {
		case "on_screen":
			tot.OnScreenS += s
		case "off_screen":
			tot.OffScreenS += s
		default:
			tot.UnknownS += s
		}
	}
	if known := tot.OnScreenS + tot.OffScreenS; known > 0 {
		p := int(tot.OnScreenS * 100 / known)
		tot.OnScreenPct = &p
	}
	if spans == nil {
		spans = []qaScreenSpan{}
	}
	return spans, tot
}

func lastSpanNow(spans []qaScreenSpan) qaScreenNow {
	if len(spans) == 0 {
		return qaScreenNow{State: "unknown", Why: "no_report"}
	}
	s := spans[len(spans)-1]
	return qaScreenNow{State: s.State, Why: s.Why, Since: s.From}
}

// offlineFrom is the device's last check-in when it has been silent longer than three
// check-in intervals (the dashboard's "offline"), else zero.
func (h *Handler) offlineFrom(lastSeen, now time.Time) time.Time {
	if now.Sub(lastSeen) > time.Duration(h.cfg.CheckinInterval()*3)*time.Second {
		return lastSeen
	}
	return time.Time{}
}

func hostAppPackage(extra json.RawMessage) string {
	var e struct {
		HostApp struct {
			Package string `json:"package"`
		} `json:"host_app"`
	}
	if json.Unmarshal(extra, &e) != nil {
		return ""
	}
	return e.HostApp.Package
}

// qaHostAppWindow is qaBatteryWindow ending now: state is not known past the present.
func qaHostAppWindow(r *http.Request) (time.Time, time.Time, error) {
	from, to, err := qaBatteryWindow(r)
	if now := time.Now(); to.After(now) {
		to = now
	}
	if err == nil && !to.After(from) {
		to = from.Add(time.Second)
	}
	return from, to, err
}

// GetTestDataDeviceHostApp is GET /api/v1/testdata/devices/{serial}/host-app: when the
// host app was on screen and when it was not, over ?range=6h|24h|7d|30d (default 24h) or
// ?start=&end= (RFC 3339, at most 31 days).
func (h *Handler) GetTestDataDeviceHostApp(w http.ResponseWriter, r *http.Request) {
	dev, ok := h.qaFirmwareDevice(w, r)
	if !ok {
		return
	}
	from, to, err := qaHostAppWindow(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	initial, events, err := h.db.HostAppScreenEvents(r.Context(), []uuid.UUID{dev.ID}, from, to)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	spans, tot := hostAppSpans(initial[dev.ID], events[dev.ID], from, to, h.offlineFrom(dev.LastSeenAt, time.Now()))
	evs := make([]qaScreenEvent, 0, len(events[dev.ID]))
	for _, e := range events[dev.ID] {
		evs = append(evs, qaScreenEvent{T: e.At.UTC(), Key: e.Key, To: e.To == "true"})
	}
	for i := range spans {
		spans[i].From, spans[i].To = spans[i].From.UTC(), spans[i].To.UTC()
	}
	now := lastSpanNow(spans)
	writeJSON(w, http.StatusOK, map[string]any{
		"serial_number": dev.SerialNumber,
		"package":       hostAppPackage(dev.LatestExtra),
		"start":         from.UTC(),
		"end":           to.UTC(),
		"last_seen_at":  dev.LastSeenAt.UTC(),
		"now":           now,
		"totals":        tot,
		"spans":         spans,
		"events":        evs,
	})
}

type qaHostAppRow struct {
	SerialNumber   string    `json:"serial_number"`
	RestaurantName string    `json:"restaurant_name"`
	Package        string    `json:"package"`
	LastSeenAt     time.Time `json:"last_seen_at"`
	qaScreenNow
	qaScreenTotals
}

// ListTestDataHostApp is GET /api/v1/testdata/host-app: every device that reports a host
// app, with its state now and its on-screen share over ?range= (default 24h, 6h|24h|7d)
// or ?start=&end= (at most 7 days here). Filters ?group= / ?restaurant=.
func (h *Handler) ListTestDataHostApp(w http.ResponseWriter, r *http.Request) {
	filter, ok := h.qaDeviceFilter(w, r)
	if !ok {
		return
	}
	from, to, err := qaHostAppWindow(r)
	if err == nil && to.Sub(from) > 7*24*time.Hour+time.Hour {
		err = errSpan7d
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	devices, err := h.db.ListDevices(r.Context(), filter, 0, 10000, "", "")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	var ids []uuid.UUID
	var keep []db.Device
	for _, d := range devices {
		if hostAppPackage(d.LatestExtra) != "" {
			ids = append(ids, d.ID)
			keep = append(keep, d)
		}
	}
	initial, events, err := h.db.HostAppScreenEvents(r.Context(), ids, from, to)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	now := time.Now()
	out := make([]qaHostAppRow, 0, len(keep))
	for _, d := range keep {
		spans, tot := hostAppSpans(initial[d.ID], events[d.ID], from, to, h.offlineFrom(d.LastSeenAt, now))
		n := lastSpanNow(spans)
		n.Since = n.Since.UTC()
		out = append(out, qaHostAppRow{SerialNumber: d.SerialNumber, RestaurantName: d.RestaurantName,
			Package: hostAppPackage(d.LatestExtra), LastSeenAt: d.LastSeenAt.UTC(), qaScreenNow: n, qaScreenTotals: tot})
	}
	writeJSON(w, http.StatusOK, map[string]any{"start": from.UTC(), "end": to.UTC(), "count": len(out), "devices": out})
}

type qaErr string

func (e qaErr) Error() string { return string(e) }

const errSpan7d = qaErr("at most 7 days per request on the list; use the per-device route for longer")
