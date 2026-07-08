package dashboard

import (
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// alertAction is the one-tap primary action offered on a humanized alert card.
type alertAction struct {
	Label string
	Href  string
	Icon  string // "logs" | "arrow"
}

// humanAlert is an alert rewritten as plain, friendly language for the inbox, the
// bell dropdown, and toasts — a headline, a sentence with the key values in bold,
// an icon key, and the most useful next action.
type humanAlert struct {
	ID          uuid.UUID
	Severity    string // critical | warning | info
	Status      string // open | acknowledged | resolved
	IconKey     string // heat|crash|offline|battery|memory|wifi|storage|charge|generic
	Headline    string
	Sentence    template.HTML
	Serial      string
	Restaurant  string
	FiredAt     time.Time
	ResolvedAt  *time.Time
	Occurrences int
	Primary     *alertAction
	CanAct      bool // whether the viewer may acknowledge/resolve (set by the handler)
}

// fnum1 renders a float without a trailing ".0" (so "55", not "55.0").
func fnum1(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%.1f", v)
}

// minsAgo turns a minute count into a compact span ("45 min", "3h", "2d").
func minsAgo(m float64) string {
	mi := int(m)
	switch {
	case mi < 60:
		return fmt.Sprintf("%d min", mi)
	case mi < 1440:
		return fmt.Sprintf("%dh", mi/60)
	default:
		return fmt.Sprintf("%dd", mi/1440)
	}
}

// humanizeAll maps a slice of raw alerts to their friendly form.
func humanizeAll(as []db.Alert) []humanAlert {
	out := make([]humanAlert, 0, len(as))
	for _, a := range as {
		out = append(out, humanizeAlert(a))
	}
	return out
}

// humanizeAlert converts one alert (its type + detail JSON + device/restaurant)
// into a friendly headline, an explanatory sentence, an icon, and a primary action.
// Everything interpolated from data is HTML-escaped; only our own <b> wrappers are
// trusted, so the result is safe to render as template.HTML.
func humanizeAlert(a db.Alert) humanAlert {
	var d map[string]any
	if len(a.Detail) > 0 {
		_ = json.Unmarshal(a.Detail, &d)
	}
	num := func(k string) float64 {
		if v, ok := d[k].(float64); ok {
			return v
		}
		return 0
	}
	str := func(k string) string {
		if v, ok := d[k].(string); ok {
			return v
		}
		return ""
	}
	b := func(s string) string { return "<b>" + s + "</b>" }

	// Location phrase, with the restaurant name bolded (escaped).
	place := "A device"
	if a.RestaurantName != "" {
		place = "A device at " + b(html.EscapeString(a.RestaurantName))
	}

	h := humanAlert{
		ID: a.ID, Severity: a.Severity, Status: a.Status,
		Serial: a.Serial, Restaurant: a.RestaurantName,
		FiredAt: a.FiredAt, ResolvedAt: a.ResolvedAt,
		Occurrences: a.Occurrences, IconKey: "generic",
	}
	var s string
	switch a.Type {
	case "overheating":
		h.IconKey, h.Headline = "heat", "A device is overheating"
		s = fmt.Sprintf("%s hit %s°C — above the %s°C limit. It may throttle or shut down if it keeps climbing.",
			place, b(fnum1(num("temp_c"))), fnum1(num("limit_c")))
	case "temp_elevated":
		h.IconKey, h.Headline = "heat", "Temperature climbing"
		s = fmt.Sprintf("%s held %s–%s°C (peaked %s°C) — trending toward throttling.",
			place, fnum1(num("temp_min")), fnum1(num("temp_max")), b(fnum1(num("peak_c"))))
	case "offline":
		h.IconKey, h.Headline = "offline", "Stopped checking in"
		s = fmt.Sprintf("%s went quiet — no report for %s. Often a Wi-Fi or power issue.", place, b(minsAgo(num("offline_minutes"))))
	case "offline_long":
		h.IconKey, h.Headline = "offline", "Offline for over an hour"
		s = fmt.Sprintf("%s hasn't reported in for %s.", place, b(minsAgo(num("offline_minutes"))))
	case "offline_peak":
		h.IconKey, h.Headline = "offline", "Offline during peak hours"
		s = fmt.Sprintf("%s dropped offline during peak service — quiet for %s.", place, b(minsAgo(num("offline_minutes"))))
	case "wifi_weak":
		h.IconKey, h.Headline = "wifi", "Weak Wi-Fi signal"
		s = fmt.Sprintf("%s has a weak Wi-Fi signal (%s dBm) — payments and voice apps may lag.", place, b(fnum1(num("rssi_dbm"))))
	case "wifi_unstable":
		h.IconKey, h.Headline = "wifi", "Wi-Fi keeps dropping"
		s = fmt.Sprintf("%s dropped Wi-Fi %s times in the last hour.", place, b(fnum1(num("disconnects_1h"))))
	case "storage_low":
		h.IconKey, h.Headline = "storage", "Storage almost full"
		s = fmt.Sprintf("%s has only %s GB free — it may stop updating or recording.", place, b(fnum1(num("storage_free_gb"))))
	case "storage_warning":
		h.IconKey, h.Headline = "storage", "Storage getting low"
		s = fmt.Sprintf("%s is down to %s GB free.", place, b(fnum1(num("storage_free_gb"))))
	case "storage_filling":
		h.IconKey, h.Headline = "storage", "Storage filling fast"
		s = fmt.Sprintf("%s dropped to %s GB free (−%s GB in 24h).", place, b(fnum1(num("today_gb"))), fnum1(num("drop_gb")))
	case "battery_low":
		h.IconKey, h.Headline = "battery", "Battery low during service"
		s = fmt.Sprintf("%s dropped to %s%% during peak hours — it may die before the shift ends.", place, b(fnum1(num("battery_pct"))))
	case "battery_high_night":
		h.IconKey, h.Headline = "battery", "Not cycling down overnight"
		s = fmt.Sprintf("%s sat at %s%% overnight instead of cycling down — that wears the battery.", place, b(fnum1(num("battery_pct"))))
	case "wlc_continuous":
		h.IconKey, h.Headline = "charge", "Stuck on the charger"
		s = fmt.Sprintf("%s has been on the wireless charger for %s straight — heat and battery stress.", place, b(minsAgo(num("span_min"))))
	case "wlc_dead":
		h.IconKey, h.Headline = "charge", "Charging pad hasn't worked"
		s = fmt.Sprintf("%s's wireless charging pad didn't work at all yesterday.", place)
	case "slow_charge_night":
		h.IconKey, h.Headline = "charge", "Charged slowly overnight"
		s = fmt.Sprintf("%s charged all night but only went %s%%→%s%% — likely a weak charger.", place, fnum1(num("first_pct")), b(fnum1(num("last_pct"))))
	case "memory_pressure":
		h.IconKey, h.Headline = "memory", "Low on memory"
		s = fmt.Sprintf("%s is using %s%% of its memory — apps may slow down or crash. A reboot usually clears it.", place, b(fnum1(num("ram_pct"))))
	case "memory_low":
		h.IconKey, h.Headline = "memory", "Very low on memory"
		s = fmt.Sprintf("%s has only %s MB of memory free — Android may start closing apps.", place, b(fnum1(num("avail_mb"))))
	case "device_crash":
		h.IconKey, h.Headline = "crash", "An app keeps crashing"
		n := int(num("count"))
		sing, plur := "crash", "crashes"
		lk := strings.ToLower(str("kind"))
		switch {
		case strings.Contains(lk, "anr"):
			sing, plur = "freeze (ANR)", "freezes (ANRs)"
		case strings.Contains(lk, "tombstone"), strings.Contains(lk, "native"):
			sing, plur = "native crash", "native crashes"
		}
		word := plur
		if n == 1 {
			word = sing
		}
		s = fmt.Sprintf("%s reported %s %s recently%s.", place, b(fmt.Sprintf("%d", n)), word, crashTail(str("summary")))
	default:
		h.Headline = alertTypeLabel(a.Type)
		s = html.EscapeString(a.Summary)
	}
	h.Sentence = template.HTML(s)

	// Primary action — a safe link to where the fix lives.
	if a.Serial != "" {
		if a.Type == "device_crash" {
			h.Primary = &alertAction{Label: "View logs", Href: "/devices/" + a.Serial + "/logcat", Icon: "logs"}
		} else {
			h.Primary = &alertAction{Label: "Open device", Href: "/devices/" + a.Serial, Icon: "arrow"}
		}
	}
	return h
}

// crashTail appends a short, escaped hint of what crashed (the DropBox summary),
// when present.
func crashTail(summary string) string {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return ""
	}
	if len(summary) > 48 {
		summary = summary[:48] + "…"
	}
	return " — " + html.EscapeString(summary)
}

// plainSentence strips the <b> wrappers and unescapes entities so a humanized
// sentence can be shown as plain text (e.g. in a JS toast via textContent).
func plainSentence(s template.HTML) string {
	r := strings.NewReplacer("<b>", "", "</b>", "")
	return html.UnescapeString(r.Replace(string(s)))
}
