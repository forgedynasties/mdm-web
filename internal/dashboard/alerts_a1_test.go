package dashboard

import (
	"strings"
	"testing"
	"time"

	"mdm/internal/alerts"
)

func causeTitle(t string) alerts.Cause { return alerts.Cause{Title: t} }

// L1 (3 Nov 2026): each problem's flags drive the rail views and chips; counts add up.
func TestAlertsL1Flags(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	needs := []problemView{
		{Key: "a", Severity: "critical", Title: "Kitchen display offline", Cause: causeTitle("Offline"), Serial: "D2S-KDS-0419", Restaurant: "Harbour Grill", RestID: "r1", Since: now.Add(-3 * time.Hour)},
		{Key: "c", Severity: "critical", Title: "Acked one", Cause: causeTitle("Offline"), Acked: true, Assignee: "me", Since: now.Add(-9 * time.Hour)},
	}
	watching := []problemView{
		{Key: "d", Severity: "warning", Title: "Battery below 15%", Devices: 4, Restaurant: "Harbour Grill", RestID: "r1", Assignee: "restaurant", Since: now.Add(-8 * time.Hour), AfterHours: true},
		{Key: "e", Severity: "warning", Title: "Storage filling up", Acked: true, NewSymptom: true, Since: now.Add(-1 * time.Hour)},
	}
	snoozed := []problemView{{Key: "f", Severity: "warning", Title: "Snoozed", Snoozed: true, Since: now.Add(-20 * time.Hour)}}
	l := buildAlertsL1("me", needs, watching, snoozed)
	if len(l.Rows) != 5 || l.Needs != 3 || l.Open != 4 || l.Acked != 1 || l.Snoozed != 1 {
		t.Fatalf("counts: rows %d needs %d open %d acked %d snoozed %d", len(l.Rows), l.Needs, l.Open, l.Acked, l.Snoozed)
	}
	if l.Crit != 2 || l.Warn != 3 || l.AtVenue != 2 || l.NoVenue != 3 || l.Mine != 1 || l.NewSince != 1 || l.Closed != 1 || l.Unassigned != 3 {
		t.Errorf("chips: %+v", l)
	}
	if l.Rows[0].P.Key != "e" {
		t.Errorf("newest first, got %s", l.Rows[0].P.Key)
	}
	if len(l.Venues) != 1 || l.Venues[0].Key != "v-r1" || l.Venues[0].N != 2 {
		t.Errorf("venues: %+v", l.Venues)
	}
	if len(l.Kinds) == 0 || l.Kinds[0].Label != "Offline" || l.Kinds[0].N != 2 {
		t.Errorf("kinds: %+v", l.Kinds)
	}
	for _, r := range l.Rows {
		if r.P.Key == "e" && (!strings.Contains(r.Flags, "needs") || !strings.Contains(r.Flags, "newsince")) {
			t.Errorf("a problem acknowledged with something new since needs action again: %q", r.Flags)
		}
		if r.P.Key == "f" && strings.Contains(r.Flags, "open") {
			t.Errorf("a snoozed problem is not in Everything open: %q", r.Flags)
		}
	}
}
