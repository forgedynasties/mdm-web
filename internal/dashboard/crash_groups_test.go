package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

func TestCrashGroupKey(t *testing.T) {
	same := [][2]string{
		// One frozen app, whichever line the parser landed on.
		{"system_app_anr|com.android.se — Process: com.android.se", "system_app_anr|com.android.se — ----- Output from /proc/pressure/memory -----"},
		{"data_app_anr|aio.app.menuboards.qa — Input dispatching timed out", "data_app_anr|aio.app.menuboards.qa:ui — Broadcast of Intent"},
		// Whitespace noise on a crash.
		{"data_app_crash|com.x.y — java.lang.NullPointerException", "data_app_crash|com.x.y — java.lang.NullPointerException  "},
	}
	for _, p := range same {
		k1, s1, _ := cut(p[0])
		k2, s2, _ := cut(p[1])
		if crashGroupKey(k1, s1) != crashGroupKey(k2, s2) {
			t.Errorf("want one group:\n  %q\n  %q", p[0], p[1])
		}
	}
	apart := [][2]string{
		// Two exceptions in one app are two problems.
		{"data_app_crash|com.x.y — java.lang.NullPointerException", "data_app_crash|com.x.y — java.lang.IllegalStateException"},
		// An ANR and a crash of the same app are not the same thing.
		{"data_app_anr|com.x.y — Input dispatching timed out", "data_app_crash|com.x.y — Input dispatching timed out"},
		// Different apps freezing.
		{"data_app_anr|com.x.y — Input dispatching timed out", "data_app_anr|com.x.z — Input dispatching timed out"},
	}
	for _, p := range apart {
		k1, s1, _ := cut(p[0])
		k2, s2, _ := cut(p[1])
		if crashGroupKey(k1, s1) == crashGroupKey(k2, s2) {
			t.Errorf("want two groups:\n  %q\n  %q", p[0], p[1])
		}
	}
}

func cut(s string) (kind, summary string, ok bool) { return strings.Cut(s, "|") }

func TestMergeCrashSignatures(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 9, 20, h, 0, 0, 0, time.UTC) }
	newest := uuid.New()
	sigs := []db.CrashSignature{
		{Kind: "system_app_anr", Summary: "com.android.se — ----- Output from /proc/pressure/memory -----", Count: 26, FirstAt: at(1), LastAt: at(20), LatestID: newest, BuildID: "b2"},
		{Kind: "system_app_anr", Summary: "com.android.se — Process: com.android.se", Count: 1942, FirstAt: at(2), LastAt: at(19), LatestID: uuid.New(), BuildID: "b1"},
		{Kind: "data_app_crash", Summary: "com.x.y — java.lang.NullPointerException", Count: 1, FirstAt: at(5), LastAt: at(5), LatestID: uuid.New()},
	}
	got := mergeCrashSignatures(sigs)
	if len(got) != 2 {
		t.Fatalf("got %d groups, want 2: %+v", len(got), got)
	}
	g := got[0]
	if g.Count != 1968 || !g.FirstAt.Equal(at(1)) || !g.LastAt.Equal(at(20)) {
		t.Errorf("merged ANR = count %d first %v last %v", g.Count, g.FirstAt, g.LastAt)
	}
	if g.LatestID != newest || g.BuildID != "b2" {
		t.Errorf("sample should be the newest event (id %v build %q)", g.LatestID, g.BuildID)
	}
	if g.Summary != "com.android.se — Process: com.android.se" {
		t.Errorf("headline should prefer a real reason over a divider line, got %q", g.Summary)
	}
	if got[1].Count != 1 || got[1].Kind != "data_app_crash" {
		t.Errorf("single crash row = %+v", got[1])
	}
}
