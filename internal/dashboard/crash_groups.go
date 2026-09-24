package dashboard

import (
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// crashGroup is one row of the device Alerts tab's crash list: every event that is
// "the same crash", with how many times it fired and when first and last. The sample
// fields are the newest event's, so the card reads like that event did before.
type crashGroup struct {
	Kind     string
	Summary  string
	BuildID  string
	LatestID uuid.UUID
	Count    int
	FirstAt  time.Time
	LastAt   time.Time
	// sampleJunk: Summary is a parser artifact, so a later clean one should replace it.
	sampleJunk bool
}

// crashGroupKey says which crash events are the same problem. A crash or native
// crash is its summary (the package and the exception line), so two different
// exceptions in one app stay two rows. An ANR is the app that froze: the second
// half of its summary is whatever line the DropBox parser happened to land on
// ("Process: com.android.se" one time, "----- Output from /proc/pressure/memory -----"
// the next), which split one frozen app into several rows.
func crashGroupKey(kind, summary string) string {
	label, _ := crashKindBadge(kind)
	if label == "ANR" {
		if pkg := extractPackageName(summary); pkg != "" {
			return kind + "\x00" + pkg
		}
	}
	return kind + "\x00" + strings.TrimSpace(summary)
}

// junkCrashSummary is a summary whose text after the package is a section divider of
// the raw report rather than a reason — shown only if nothing better exists.
func junkCrashSummary(summary string) bool {
	_, rest, ok := strings.Cut(summary, " — ")
	return ok && strings.HasPrefix(strings.TrimSpace(rest), "-----")
}

// mergeCrashSignatures folds exact (kind, summary) signatures into crash groups by
// crashGroupKey, newest group first. Input order does not matter.
func mergeCrashSignatures(sigs []db.CrashSignature) []crashGroup {
	idx := map[string]int{}
	var out []crashGroup
	for _, s := range sigs {
		k := crashGroupKey(s.Kind, s.Summary)
		i, ok := idx[k]
		if !ok {
			idx[k] = len(out)
			out = append(out, crashGroup{
				Kind: s.Kind, Summary: s.Summary, BuildID: s.BuildID, LatestID: s.LatestID,
				Count: s.Count, FirstAt: s.FirstAt, LastAt: s.LastAt, sampleJunk: junkCrashSummary(s.Summary),
			})
			continue
		}
		g := &out[i]
		g.Count += s.Count
		if s.FirstAt.Before(g.FirstAt) {
			g.FirstAt = s.FirstAt
		}
		if s.LastAt.After(g.LastAt) {
			g.LastAt, g.BuildID, g.LatestID = s.LastAt, s.BuildID, s.LatestID
		}
		// The headline prefers a real reason over a divider line, newest first.
		junk := junkCrashSummary(s.Summary)
		if (g.sampleJunk && !junk) || (junk == g.sampleJunk && s.LastAt.Equal(g.LastAt)) {
			g.Summary, g.sampleJunk = s.Summary, junk
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastAt.After(out[j].LastAt) })
	return out
}
