package dashboard

import (
	"strings"
	"testing"
	"time"

	"mdm/internal/db"
)

func TestVenueLostView(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	if v := lostViewOf(db.VenueLostWatch{}, "", now); v.Paused || v.Summary != "Watching" || v.SummarySub != "alerts when a tablet leaves" {
		t.Errorf("default = %+v", v)
	}
	if v := lostViewOf(db.VenueLostWatch{SelfLockMinutes: 10}, "", now); v.SummarySub != "alerts, and locks itself after 10 min" {
		t.Errorf("self-lock = %+v", v)
	}
	until := now.Add(72 * time.Hour)
	v := lostViewOf(db.VenueLostWatch{SelfLockMinutes: 10, Paused: true, PausedUntil: &until}, "America/Los_Angeles", now)
	if !v.Paused || v.Summary != "Paused" || v.PausedSub != "resumes Sun 25 Oct, 05:00" {
		t.Errorf("paused = %+v", v)
	}
	past := now.Add(-time.Minute)
	if v := lostViewOf(db.VenueLostWatch{Paused: true, PausedUntil: &past}, "", now); v.Paused {
		t.Error("an ended pause still holds")
	}
	if v := lostViewOf(db.VenueLostWatch{Paused: true}, "", now); v.PausedSub != "until someone resumes" {
		t.Errorf("open-ended = %+v", v)
	}
}

func TestVenueLostPauseTodo(t *testing.T) {
	if _, ok := venueLostPauseTodo(venueLostView{}); ok {
		t.Error("a watching venue has a pause line")
	}
	td, ok := venueLostPauseTodo(venueLostView{Paused: true, PausedSub: "until someone resumes", By: "by ali · 22 Oct"})
	if !ok || td.Title != "Lost-device watch paused" || td.Href != "?open=lost#vn-lost" || !strings.Contains(td.Sub, "by ali") {
		t.Errorf("todo = %+v", td)
	}
}
