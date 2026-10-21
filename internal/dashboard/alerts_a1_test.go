package dashboard

import (
	"strings"
	"testing"
	"time"

	"mdm/internal/db"
)

func TestAlertBannerOrder(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	needs := []problemView{
		{Key: "a", Severity: "critical", Title: "Kitchen display offline", Serial: "D2S-KDS-0419", Restaurant: "Harbour Grill", Since: now.Add(-3 * time.Hour)},
		{Key: "b", Severity: "critical", Title: "Device offline", Serial: "T7-1", Since: now.Add(-5 * time.Hour), AfterHours: true},
		{Key: "c", Severity: "critical", Title: "Acked one", Acked: true, Since: now.Add(-9 * time.Hour)},
	}
	watching := []problemView{
		{Key: "d", Severity: "warning", Title: "Battery below 15%", Devices: 4, Restaurant: "Harbour Grill", Assignee: "restaurant", Since: now.Add(-8 * time.Hour)},
		{Key: "e", Severity: "warning", Title: "Storage filling up", Since: now.Add(-1 * time.Hour)},
		{Key: "f", Severity: "warning", Title: "Snoozed", Snoozed: true, Since: now.Add(-20 * time.Hour)},
	}
	b := buildAlertBanner(needs, watching, now)
	if b.Tone != "bad" || b.Headline != "4 to handle" || len(b.Todos) != 3 || b.More != 1 {
		t.Fatalf("banner = %+v", b)
	}
	// Critical while open first, then critical after hours, then the oldest warning.
	if b.Todos[0].Title != "Kitchen display offline at Harbour Grill" || b.Todos[1].Title != "Device offline" ||
		b.Todos[2].Title != "Battery below 15% at Harbour Grill" {
		t.Errorf("order = %q, %q, %q", b.Todos[0].Title, b.Todos[1].Title, b.Todos[2].Title)
	}
	if !strings.Contains(b.Todos[0].Sub, "nobody assigned") || !strings.Contains(b.Todos[1].Sub, "raised after hours") ||
		!strings.Contains(b.Todos[2].Sub, "4 devices") || !strings.Contains(b.Todos[2].Sub, "assigned to the restaurant") {
		t.Errorf("subs = %q / %q / %q", b.Todos[0].Sub, b.Todos[1].Sub, b.Todos[2].Sub)
	}
	if b.Todos[0].Href != "#"+alAnchor("a") || strings.ContainsAny(alAnchor("restaurant:Mom's Biryani"), " :'") {
		t.Errorf("anchors: %q, %q", b.Todos[0].Href, alAnchor("restaurant:Mom's Biryani"))
	}

	calm := buildAlertBanner([]problemView{{Severity: "warning", Acked: true}}, nil, now)
	if calm.Tone != "calm" || calm.Headline != "Nothing waiting on you" || !strings.Contains(calm.Calm, "acknowledged") {
		t.Errorf("calm = %+v", calm)
	}
}

func TestAlertCardsAndRail(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	needs := []problemView{{Severity: "critical", Since: now.Add(-3 * time.Hour)}}
	watching := []problemView{{Severity: "warning", Acked: true, Assignee: "sam"}, {Severity: "warning", Since: now.Add(-2 * time.Hour)}}
	raised := &db.AlertsRaised{Critical24: 1, CriticalPrev: 3, Warning24: 4, WarningPrev: 4}
	crash := &db.FleetCrashStats{Total24h: 9, Devices24h: 4, Daily: []int{1, 2, 0, 3, 2, 5, 3}}
	cards := buildAlertCards(needs, watching, raised, crash, now)
	if len(cards) != 3 {
		t.Fatalf("cards = %d", len(cards))
	}
	if c := cards[0]; c.A.Value != "1" || c.B.Value != "1" || c.B.Delta != "▼ 2" || c.B.Tone != "good" {
		t.Errorf("critical = %+v", c)
	}
	if c := cards[1]; c.A.Value != "2" || c.B.Delta != "" || c.B.Tone != "flat" {
		t.Errorf("warnings = %+v (no change, no arrow)", c)
	}
	if c := cards[2]; c.A.Value != "9" || c.A.Delta != "▼ 2" || c.B.Value != "4" {
		t.Errorf("crashes = %+v", c)
	}
	// Without fleet counts (a scoped viewer, a device filter) there is no invented delta.
	scoped := buildAlertCards(needs, watching, nil, nil, now)
	if len(scoped) != 2 || scoped[0].B.Label != "oldest" || scoped[0].B.Value != "3h" || scoped[1].B.Label != "acknowledged" {
		t.Errorf("scoped = %+v", scoped)
	}

	r := buildAlertRail(needs, watching, crash, now)
	if r.Total != 3 || r.Acked != 1 || r.Pct != 33 || r.Owned != 1 || r.OldestUnowned != "3h" || r.Tone != "danger" {
		t.Errorf("rail = %+v", r)
	}
	if len(r.Bars) != 7 || r.CrashWeek != 16 || !r.Bars[6].Cur || r.Bars[2].Pct != 4 {
		t.Errorf("bars = %+v week %d", r.Bars, r.CrashWeek)
	}
	if v := alVenues([]problemView{{Restaurant: "B"}, {Restaurant: "A"}}, []problemView{{Restaurant: "B"}, {}}); len(v) != 2 || v[0] != "A" {
		t.Errorf("venues = %v", v)
	}
}
