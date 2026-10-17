package dashboard

import (
	_ "image/gif"
	_ "image/jpeg"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"mdm/internal/db"
)

// activityActor is one option in the Activity page's "User" filter — value is
// the stable username the row is actually filtered/stored by, label is the
// user's current display name.
type activityActor struct {
	Username string
	Name     string
	Count    int // log entries by this actor (page views excluded)
}

// ActivityPage renders the admin-only activity log — every audit entry, optionally
// filtered to one user, so an admin can see everything at once or drill into one
// person's actions.
func (h *Handler) ActivityPage(w http.ResponseWriter, r *http.Request) {
	// actor is the username selected in the "User" filter dropdown (see below).
	actor := r.URL.Query().Get("actor")
	isAdmin := h.role(r) == "admin"
	// The "Show admin" and "Include page views" toggles are admin-only: everyone
	// else always sees the list without admin actions and without page views.
	showAdmin := isAdmin && r.URL.Query().Get("admin") == "1"
	excludeActor := ""
	if !showAdmin {
		excludeActor = "admin"
	}
	// Fetch broadly (everything but a straight exclude-admin) and apply the actor
	// filter AFTER resolving names below, rather than as a SQL exact-match on the
	// raw column. audit_log.actor is a mix of formats: current rows store a
	// username, but rows logged before a user had a name on file fell back to
	// storing the username too (same value, coincidentally matches), while rows
	// logged after they'd set a name stored that display-name text directly (a
	// pre-this-fix snapshot) — a raw exact-match against "the selected user's
	// username" only ever caught the first case, silently dropping the rest of
	// that person's own history. Matching on the resolved display name instead
	// catches all three shapes.
	showViews := isAdmin && r.URL.Query().Get("views") == "1"
	// device= filters to one device's activity. Exact serial match on the device the
	// action was recorded against, so bulk actions and rollout steps land on every
	// device they actually touched rather than only where the serial happens to be
	// spelled out in `target`.
	deviceSerial := strings.TrimSpace(r.URL.Query().Get("device"))
	entries, err := h.db.ListAuditForActivity(r.Context(), excludeActor, showViews, 1000, deviceSerial)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	deviceSerials, _ := h.db.ListAuditDeviceSerials(r.Context())
	// The actor filter only lists real team accounts (users with an @aioapp.com
	// email), not raw audit_log.actor noise like "System"/"API key"/scheduled-recipe
	// labels or stale entries from accounts that no longer exist.
	users, err := h.db.ListUsers(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	var actors []activityActor
	nameByUsername := make(map[string]string, len(users))
	counts, _ := h.db.AuditCountsByActor(r.Context())
	for _, u := range users {
		nameByUsername[u.Username] = u.DisplayName()
		if u.Email != nil && strings.HasSuffix(strings.ToLower(*u.Email), "@aioapp.com") {
			actors = append(actors, activityActor{Username: u.Username, Name: u.DisplayName(), Count: counts[u.Username]})
		}
	}
	// Most active first; the filter shows the top five and folds the rest (A–Z).
	sort.SliceStable(actors, func(i, j int) bool {
		if actors[i].Count != actors[j].Count {
			return actors[i].Count > actors[j].Count
		}
		return strings.ToLower(actors[i].Name) < strings.ToLower(actors[j].Name)
	})
	topActors, moreActors := actors, []activityActor(nil)
	if len(actors) > 5 {
		topActors, moreActors = actors[:5], append([]activityActor(nil), actors[5:]...)
		sort.SliceStable(moreActors, func(i, j int) bool { return strings.ToLower(moreActors[i].Name) < strings.ToLower(moreActors[j].Name) })
	}
	actorName := actor
	if dn, ok := nameByUsername[actor]; ok {
		actorName = dn
	}
	for i := range entries {
		if dn, ok := nameByUsername[entries[i].Actor]; ok {
			entries[i].Actor = dn
		}
	}
	if actor != "" {
		kept := make([]db.AuditEntry, 0, len(entries))
		for _, e := range entries {
			if e.Actor == actorName {
				kept = append(kept, e)
			}
		}
		entries = kept
	}

	// KPI strip: today / this-week counts, distinct users, most common action —
	// computed over the same (already actor-filtered) entry set the table shows.
	now := time.Now().UTC()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	weekStart := todayStart.AddDate(0, 0, -6)
	entriesToday, entriesWeek := 0, 0
	distinctUsers := map[string]bool{}
	actionCounts := map[string]int{}
	for _, e := range entries {
		if !e.CreatedAt.Before(todayStart) {
			entriesToday++
		}
		if !e.CreatedAt.Before(weekStart) {
			entriesWeek++
		}
		distinctUsers[e.Actor] = true
		actionCounts[e.Action]++
	}
	topAction, topActionN := "—", 0
	for a, n := range actionCounts {
		if n > topActionN || (n == topActionN && a < topAction) {
			topAction, topActionN = a, n
		}
	}

	const pageSize = 40
	total := len(entries)
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages < 1 {
		totalPages = 1
	}
	page := 1
	if p, e := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("page"))); e == nil && p > 0 {
		page = p
	}
	if page > totalPages {
		page = totalPages
	}
	start := (page - 1) * pageSize
	end := start + pageSize
	if end > total {
		end = total
	}
	var pageEntries []db.AuditEntry
	if total > 0 {
		pageEntries = entries[start:end]
	}

	h.render(w, r, "activity.html", map[string]any{
		"UsersTab":      "activity",
		"Entries":       pageEntries,
		"Total":         total,
		"Page":          page,
		"TotalPages":    totalPages,
		"Actors":        actors,
		"TopActors":     topActors,
		"MoreActors":    moreActors,
		"Actor":         actor,
		"ActorName":     actorName,
		"Device":        deviceSerial,
		"DeviceSerials": deviceSerials,
		"ShowAdmin":     showAdmin,
		"ShowViews":     showViews,
		"EntriesToday":  entriesToday,
		"EntriesWeek":   entriesWeek,
		"DistinctUsers": len(distinctUsers),
		"TopAction":     topAction,
		"TopActionN":    topActionN,
	})
}
