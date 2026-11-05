package dashboard

import (
	"context"
	"sort"
	"strings"
	"time"

	"mdm/internal/db"
)

// Activity tracks (T1, picked 5 Nov 2026; user: "the activity is polluted by app follow qa
// api entries, we should have a separate track for auto job and qa api"). Every action is
// People (a person's account: an email), API & QA (the admin API key, the QA API) or
// Automations (the server's own jobs: app follow, auto-update, role app installs, auto-pull,
// the control probe, maintenance…). People › Activity and the device page's Activity tab
// open on People; Automations and API & QA lead with one row per job or key.
const (
	trackPeople = "people"
	trackAuto   = "auto"
	trackAPI    = "api"
)

var apiActors = map[string]bool{"api": true, "api key": true, "qa api": true, "qa-api": true, "admin api": true}

func actorTrack(actor string) string {
	n := strings.ToLower(strings.TrimSpace(actor))
	switch {
	case apiActors[n]:
		return trackAPI
	case strings.Contains(n, "@"):
		return trackPeople
	}
	return trackAuto
}

func normTrack(t string) string {
	if t == trackAuto || t == trackAPI {
		return t
	}
	return trackPeople
}

// trackSource is one job or key in the Automations / API & QA tracks: what it sent in the
// window, how much failed, and when it last did anything.
type trackSource struct {
	Name, About  string
	Sent, Failed int
	OkPct        int
	Last         time.Time
}

var trackAbout = map[string][2]string{
	"app-follow":       {"App follow", "keeps devices on the channels they follow"},
	"auto-update":      {"Client auto-update", "MDM clients to the hosted build"},
	"role-app-install": {"Role app installs", "the prod app where it is missing"},
	"control probe":    {"Control probe", "checks each device still takes commands"},
	"auto-pull":        {"Library auto-pull", "newest app builds pulled from devices"},
	"key-clash":        {"Key clash retries", "reinstall after a signing-key clash"},
	"maintenance":      {"Maintenance", "the server's housekeeping"},
	"qa api":           {"QA API", "the test rig: open and close apps, screen, wireless charging"},
	"api key":          {"Admin API key", "scripts and tools using the admin key"},
	"api":              {"Admin API", "calls made with the admin key"},
}

// trackSources sums the window's commands by who sent them, for one track.
func (h *Handler) trackSources(ctx context.Context, track string, since time.Time) []trackSource {
	rows, err := h.db.CommandsBySender(ctx, since)
	if err != nil {
		return nil
	}
	var out []trackSource
	for _, r := range rows {
		if actorTrack(r.By) != track {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(r.By))
		s := trackSource{Name: r.By, Sent: r.Sent, Failed: r.Failed, Last: r.Last}
		if a, ok := trackAbout[key]; ok {
			s.Name, s.About = a[0], a[1]
		} else if l, ok := db.SystemActorLabel(r.By); ok {
			s.Name = l
		} else if r.By == "" {
			s.Name = "System"
		}
		if s.Sent > 0 {
			s.OkPct = (s.Sent - s.Failed) * 100 / s.Sent
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Sent > out[j].Sent })
	return out
}
