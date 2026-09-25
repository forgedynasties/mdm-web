package dashboard

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

func TestBuildAttention(t *testing.T) {
	now := time.Now()
	later := now.Add(time.Hour)
	alert := func(typ, sev, serial, rest string, fired time.Time) db.Alert {
		id := uuid.New()
		return db.Alert{ID: id, Type: typ, Severity: sev, Status: "open", Serial: serial, RestaurantName: rest, FiredAt: fired}
	}
	muted := alert("offline", "critical", "T7-9", "Elsewhere", now)
	muted.MutedUntil = &later
	alerts := []db.Alert{
		alert("overheating", "warning", "T7-3", "Golden Wok", now),
		alert("offline", "critical", "T7-1", "Pho 88", now),
		alert("offline", "critical", "T7-2", "Pho 88", now.Add(-30*time.Minute)),
		muted,
	}
	rows, total, crit := buildAttention(alerts, 5)
	if total != 3 || crit != 1 || len(rows) != 3 {
		t.Fatalf("total=%d crit=%d rows=%d, want 3/1/3", total, crit, len(rows))
	}
	if r := rows[0]; r.Severity != "critical" || r.Devices != 2 || r.Serial != "" || r.Restaurant != "Pho 88" || r.Issue != "Device offline" {
		t.Errorf("folded offline row = %+v", r)
	}
	if !rows[0].Since.Equal(now.Add(-30 * time.Minute)) {
		t.Errorf("since should be the oldest member, got %v", rows[0].Since)
	}
	if r := rows[1]; r.Severity != "warning" || r.Serial != "T7-3" || r.Href != "/alerts?device=T7-3" {
		t.Errorf("single-device row = %+v", r)
	}
	if r := rows[2]; r.Severity != "info" || r.Devices != 5 || r.Href != "/enrollment" {
		t.Errorf("inbox row = %+v", r)
	}
}

func TestMainIssue(t *testing.T) {
	if got := mainIssue([]string{"2 of 6 offline (−13)", "x"}); got != "2 of 6 offline" {
		t.Errorf("mainIssue = %q", got)
	}
	if got := mainIssue(nil); got != "" {
		t.Errorf("mainIssue(nil) = %q", got)
	}
}
