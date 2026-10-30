package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	_ "image/gif"
	_ "image/jpeg"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"mdm/internal/db"
	"mdm/internal/product"
	"mdm/internal/version"

	"github.com/google/uuid"
)

// Landing renders the entry page for staff: what the MDM does, guides, FAQ, the
// training videos (streamed from S3 via short-lived presigned URLs), sign in, sign up.
func (h *Handler) Landing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	h.tmpl.ExecuteTemplate(w, "landing.html", map[string]any{
		"Brand":         h.cfg.CustomBrand(),
		"AssetVer":      h.assetVer,
		"SignupEnabled": true,
		"Training":      h.trainingVideos(r.Context()),
	})
}

// SneakPeek renders a public, no-account preview of the dashboard: the REAL
// Overview template (overview_new.html, the one "/" renders) and chrome, fed from the
// sample fleet in sneak_peek_sample.go instead of the database. Reachable from the
// landing page's "Take a sneak peek" button.
//
// It reuses overviewViewModel and the new Overview's own builders (venue rows,
// attention, compliance, clean up), so the preview is the real page, not a mock, and
// cannot drift from it. A synthetic viewer session is injected for the duration of
// this one request so the role-gated chrome renders; no cookie, no DB session, and
// none of the numbers come from real devices. Viewer (not admin) so the preview only
// offers what it can actually show.
func (h *Handler) SneakPeek(w http.ResponseWriter, r *http.Request) {
	r = sneakPeekAuth(r)
	s := newSneakPeekSample(time.Now())
	data := h.sneakPeekOverview(r, s, h.cfg.CheckinInterval()*3)
	h.sneakPeekChrome(data, "overview", s.OpenAlerts())
	h.sneakPeekRender(w, "overview_new.html", data)
}

// sneakPeekOverview is the Overview's data from the sample: the shared view model,
// then what overviewNewData adds for the live page. Reads nothing from the database.
func (h *Handler) sneakPeekOverview(r *http.Request, s *sneakPeekSample, activeSecs int) map[string]any {
	summary, groups := s.Summary(), s.Groups()
	data := h.overviewViewModel(r, summary, groups, s.Hot(), s.Daily(summary), s.OpenAlerts(), s.CrashStats(),
		nil, s.Rollouts(), nil, activeSecs, nil, s.Inbox())
	cc, classOn := s.ClassCounts()
	data["Products"] = compRolesFrom(cc)
	att, attN, attCrit := buildAttention(s.alerts, s.Inbox())
	data["AttentionRows"], data["AttentionTotal"], data["AttentionCritical"] = att, attN, attCrit
	data["ClassOnline"] = classOnlineRows(classOn)
	data["Hygiene"] = hygieneRows(s.Hygiene(summary))
	data["Compliance"] = s.Compliance()

	// Overview C: the filters work in the preview too, against the sample.
	devs := s.OvDevs()
	byCount := append([]db.ClassOnline(nil), classOn...)
	sort.SliceStable(byCount, func(i, j int) bool { return byCount[i].Total > byCount[j].Total })
	var classes []string
	for _, c := range byCount {
		classes = append(classes, c.Class)
	}
	var venueOpts []ovVenueOpt
	for i, v := range spVenues {
		venueOpts = append(venueOpts, ovVenueOpt{sneakPeekVenueID(i), v.name})
	}
	sc := parseOvScope(r, "/sneak-peek", classes, venueOpts)
	tr := s.Trend(summary)
	alerts := ovScopeAlerts(s.alerts, devs, sc)
	att, attN, attCrit = buildAttention(alerts, ovScopeInbox(s.Inbox(), devs, sc))
	data["AttentionRows"], data["AttentionTotal"], data["AttentionCritical"] = att, attN, attCrit
	online, total := 0, 0
	has := map[uuid.UUID]bool{}
	for _, d := range devs {
		if sc.keeps(d.Class, d.Venue) {
			total++
			if d.Online {
				online++
			}
			if d.Venue != nil {
				has[*d.Venue] = true
			}
		}
	}
	var scopedGroups []db.GroupHealth
	for _, g := range groups {
		if !sc.On || has[g.GroupID] {
			scopedGroups = append(scopedGroups, g)
		}
	}
	venues := s.Venues(groups)
	if sc.On {
		rows := venues.Rows[:0]
		for _, v := range venues.Rows {
			if id, err := uuid.Parse(v.ID); err == nil && has[id] {
				rows = append(rows, v)
			}
		}
		venues.Rows, venues.Total, venues.NeedLook = rows, len(rows), 0
		for _, v := range rows {
			if v.NeedsLook {
				venues.NeedLook++
			}
		}
		if sc.Venue != nil {
			venues.Unplaced = 0
		}
	}
	data["Venues"] = venues
	data["Scope"] = sc
	data["OvBanner"] = buildOvBanner(att, attN, scopedGroups, s.CrashStats().ByRestaurant, ovHotVenues(devs), tr, sc, online, total)
	data["OvStats"] = buildOvStats(tr, devs, sc, s.now)
	data["OvPills"] = buildOvPills(devs, alerts, sc)
	data["OvRollouts"] = buildOvRollouts(s.RolloutBars(), s.now, false)
	if summary.Total > 0 {
		data["OnlinePct"] = fmt.Sprintf("%.1f", float64(summary.RecentlyActive)*100/float64(summary.Total))
	}
	return data
}

// sneakPeekAuth injects a synthetic viewer session for the life of one preview
// request, so the role-gated chrome (dock, name, access) renders without a real
// cookie/DB session. Viewer, so write actions are hidden/role-locked.
func sneakPeekAuth(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxSessionKey{}, &db.Session{
		Username: "guest", Role: "viewer",
		ExpiresAt: time.Now().Add(time.Hour), LastSeen: time.Now(),
	}))
}

// sneakPeekChrome fills the layout chrome keys a preview page needs when rendered
// directly (not via h.render, so nothing touches the DB or writes an audit row).
func (h *Handler) sneakPeekChrome(data map[string]any, active string, openAlerts int) {
	data["Role"] = "viewer"
	data["NavRole"] = "viewer" // the layout reads the chrome role (render.go sets it for real pages)
	data["Boosted"] = false
	data["CurrentUser"] = "Guest preview"
	if userBubbleFn != nil {
		data["CurrentBubble"] = userBubbleFn("Guest")
	}
	data["Brand"] = h.cfg.CustomBrand()
	data["Use24Hour"] = h.cfg.Use24Hour()
	data["Version"] = version.Current()
	data["AssetVer"] = h.assetVer
	data["ActivePage"] = active
	data["ShowTour"] = false
	data["AlertsOpenCount"] = openAlerts
	data["Preview"] = true
}

// sneakPeekRender writes a preview page from the real template + synthetic data.
func (h *Handler) sneakPeekRender(w http.ResponseWriter, name string, data map[string]any) {
	w.Header().Set("Cache-Control", "no-store")
	var buf bytes.Buffer
	if err := h.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("sneak-peek render %s: %v", name, err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	_, _ = buf.WriteTo(w)
}

// SneakPeekAlerts is the preview's dummy Alerts inbox: the real alerts.html rendered
// against the sample fleet's alerts, so the dock's Alerts item is navigable in the preview.
func (h *Handler) SneakPeekAlerts(w http.ResponseWriter, r *http.Request) {
	r = sneakPeekAuth(r)
	s := newSneakPeekSample(time.Now())
	data := s.AlertsPage()
	h.sneakPeekChrome(data, "alerts", s.OpenAlerts())
	h.sneakPeekRender(w, "alerts.html", data)
}

// SneakPeekFleet is the preview's dummy Fleet roster: the real devices.html rendered
// against a synthetic device list, so the dock's Fleet item is navigable in the preview.
func (h *Handler) SneakPeekFleet(w http.ResponseWriter, r *http.Request) {
	r = sneakPeekAuth(r)
	s := newSneakPeekSample(time.Now())
	devices, online := s.Devices()
	summary := s.Summary()
	n := summary.Total
	data := map[string]any{
		"Title": "Devices", "Devices": devices, "Online": online,
		"Nicknames": map[uuid.UUID]string{}, "Flapping": map[uuid.UUID]bool{},
		"Total": n, "FleetTotal": n, "FilterCount": 0, "InactiveCount": 0,
		"RailGroups": []any{}, "RailRestaurants": []any{}, "RailReleases": []any{}, "RailProducts": []any{},
		"Groups": []any{}, "Restaurants": []any{}, "Productions": []any{}, "Builds": []any{}, "Timezones": []any{},
		"Products": product.All(), "Classes": product.Classes(),
		"SelectedCollection": "All devices", "SelectedCount": n,
		"ActiveRestaurant": nil, "ActiveGroup": nil, "ActiveReleaseID": 0,
		"View": "", "ViewID": "", "ViewColl": "",
		"Page": 1, "TotalPages": 1, "Query": "", "PageSize": 200,
		"Summary": summary, "Sort": "", "SortDir": "",
		"FilterRestaurant": "", "FilterGroup": "", "FilterProduction": "", "FilterProduct": "",
		"FilterStatus": "", "FilterBuild": "", "FilterBattery": "", "FilterRAM": "", "FilterTemp": "", "FilterKiosk": "",
		"FilterCharging": "", "FilterTimezone": "", "FilterKind": "", "FilterClass": "",
		"FilterOnboarding": "", "FilterLifecycle": "", "FilterHidden": false,
		"ActiveThresholdSecs": 180, "ActiveThresholdLabel": "3 min",
		"Density": h.cfg.Density(), "MapsEmbedKey": "",
	}
	h.sneakPeekChrome(data, "devices", s.OpenAlerts())
	h.sneakPeekRender(w, "devices.html", data)
}

// trainingChapter is one entry of training/manifest.json in the S3 bucket (written by
// tools/reel/publish-training.sh) plus presigned URLs for the page.
type trainingChapter struct {
	ID       string `json:"id"`
	Number   int    `json:"number"`
	Title    string `json:"title"`
	Intro    string `json:"intro"`
	Steps    int    `json:"steps"`
	Duration int    `json:"duration"` // seconds
	Video    string `json:"-"`
	Poster   string `json:"-"`
}

const trainingPresignTTL = 6 * time.Hour

var (
	trainingMu     sync.Mutex
	trainingCache  []trainingChapter
	trainingCached time.Time
)

// trainingVideos returns the chapters with presigned URLs, cached for 5 minutes so the
// landing page does not hit S3 on every visit. Empty when S3 is not configured or the
// manifest is missing; the template hides the section then.
func (h *Handler) trainingVideos(ctx context.Context) []trainingChapter {
	if h.apk == nil {
		return nil
	}
	trainingMu.Lock()
	defer trainingMu.Unlock()
	if time.Since(trainingCached) < 5*time.Minute {
		return trainingCache
	}
	trainingCached = time.Now() // also caches a miss, so a missing manifest is cheap
	body, _, _, _, err := h.apk.Get(ctx, "training/manifest.json", "")
	if err != nil {
		trainingCache = nil
		return nil
	}
	defer body.Close()
	var m struct {
		Chapters []trainingChapter `json:"chapters"`
	}
	if err := json.NewDecoder(body).Decode(&m); err != nil {
		log.Printf("[landing] training manifest: %v", err)
		trainingCache = nil
		return nil
	}
	out := m.Chapters[:0]
	for _, c := range m.Chapters {
		v, err1 := h.apk.PresignGet(ctx, "training/"+c.ID+".mp4", trainingPresignTTL)
		p, err2 := h.apk.PresignGet(ctx, "training/"+c.ID+".jpg", trainingPresignTTL)
		if err1 != nil {
			continue
		}
		c.Video = v
		if err2 == nil {
			c.Poster = p
		}
		out = append(out, c)
	}
	trainingCache = out
	return out
}
