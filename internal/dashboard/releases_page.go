package dashboard

import (
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"mdm/internal/db"
	"mdm/internal/product"
)

// ── Releases (/releases; O2, picked 16 Oct) ──────────────────────
//
// One page for what used to be three (/rollouts, /updates/rollouts, /releases), in the
// dock as Releases: the
// release list on the left — every release and every build devices report without one —
// and the selected release beside it, with its changelog, packages, how much of the fleet
// runs it and each rollout of it. Anything rolling out right now is pinned on top. The
// release page and the rollout page stay; this page opens them.

type rolloutView struct {
	URL                                  string
	ID                                   int
	Version                              string
	Status                               string
	CreatedAt                            time.Time
	CreatedBy                            string
	Legacy                               bool
	Total, Installed, Installing, Failed int
	// NotBack: rebooted to apply it 15+ minutes ago and not confirmed on the new build. A
	// device waiting for or in its applying reboot otherwise counts as installing, not
	// installed (user, 23 Oct 2026: #422 read "26 of 26 all installed" with one tablet that
	// never came back from its reboot).
	NotBack int
}

// Pending is what has not started or finished either way.
func (v rolloutView) Pending() int {
	n := v.Total - v.Installed - v.Installing - v.Failed - v.NotBack
	if n < 0 {
		return 0
	}
	return n
}

// The running-updates card at the top of /releases (U1, picked 23 Oct 2026; user: "doesn't
// look good" about five identical "Updating" boxes): one card, aligned rows, and a state word
// that says which one needs a look. A rollout whose devices are all done reads "Finished" at
// the bottom instead of "Updating"; one with nothing moving reads "Waiting" (its devices have
// not checked in since it was sent).
type roRow struct {
	rolloutView
	State string // look | run | wait | done
	Note  string
}

type roStrip struct {
	Rows                                  []roRow
	Running, Installed, Total, Installing int // across the rows not yet done
}

func buildRoStrip(active []rolloutView) roStrip {
	var st roStrip
	var done []roRow
	for _, v := range active {
		r := roRow{rolloutView: v}
		failed := ""
		if v.NotBack > 0 {
			failed += fmt.Sprintf(" · %d not back from %s reboot", v.NotBack, dp1Their(v.NotBack))
		}
		if v.Failed > 0 {
			failed += fmt.Sprintf(" · %d failed", v.Failed)
		}
		switch {
		case v.Total > 0 && v.Installed+v.Failed >= v.Total:
			r.State = "done"
			r.Note = "all installed"
			if v.Failed > 0 {
				r.Note = fmt.Sprintf("%d failed", v.Failed)
			}
			done = append(done, r)
			continue
		case v.NotBack > 0 && v.Installing == 0:
			r.State = "look"
			r.Note = strings.TrimPrefix(failed, " · ")
			if p := v.Pending(); p > 0 {
				r.Note += fmt.Sprintf(" · %d haven't checked in", p)
			}
		case v.Installing > 0:
			r.State = "run"
			r.Note = fmt.Sprintf("%d installing", v.Installing) + failed
		default:
			r.State = "wait"
			r.Note = fmt.Sprintf("%d haven't checked in", v.Pending()) + failed
		}
		st.Running++
		st.Installed += v.Installed
		st.Total += v.Total
		st.Installing += v.Installing
		st.Rows = append(st.Rows, r)
	}
	st.Rows = append(st.Rows, done...)
	return st
}

// releasesRedirect sends the old list addresses to the one page, query and all.
func releasesRedirect(w http.ResponseWriter, r *http.Request) {
	u := "/releases"
	if q := r.URL.RawQuery; q != "" {
		u += "?" + q
	}
	http.Redirect(w, r, u, http.StatusMovedPermanently)
}

func deploymentView(d db.Update) rolloutView {
	v := rolloutView{URL: fmt.Sprintf("/releases/%d/deployments/%d", d.ReleaseID, d.ID), ID: d.ID, Status: d.Status,
		CreatedAt: d.CreatedAt, CreatedBy: d.CreatedBy, Total: d.DeviceTotal, Installed: d.DeviceInstalled,
		Installing: d.DeviceDownloading + d.DeviceInstalling + d.DeviceAwaiting - d.DeviceNotBack, Failed: d.DeviceFailed,
		NotBack: d.DeviceNotBack}
	if d.Release != nil {
		v.Version = d.Release.Version
	}
	return v
}

func legacyView(d db.LegacyDeployment) rolloutView {
	return rolloutView{URL: fmt.Sprintf("/updates/legacy/deployments/%d", d.ID), ID: d.ID, Version: d.ReleaseVersion, Status: d.Status,
		CreatedAt: d.CreatedAt, CreatedBy: d.CreatedBy, Legacy: true, Total: d.Total, Installed: d.Installed,
		Installing: d.Installing, Failed: d.Failed}
}

// mergeRollouts groups every rollout by release and picks the running ones for the
// pinned strip. A legacy half of a fleet rollout is folded into that rollout's row.
func mergeRollouts(deployments []db.Update, legacy []db.LegacyDeployment, productMatches func(string) bool) (byRelease map[int][]rolloutView, active []rolloutView) {
	byRelease = map[int][]rolloutView{}
	for _, d := range deployments {
		if !productMatches(d.Product) {
			continue
		}
		byRelease[d.ReleaseID] = append(byRelease[d.ReleaseID], deploymentView(d))
	}
	// A legacy half of a fleet rollout is that rollout's own devices: fold it into its row.
	where := map[int][2]int{} // update id → (release id, index in byRelease)
	for rid, rs := range byRelease {
		for i, v := range rs {
			if !v.Legacy {
				where[v.ID] = [2]int{rid, i}
			}
		}
	}
	for _, d := range legacy {
		if d.UpdateID != nil {
			if at, ok := where[*d.UpdateID]; ok {
				v := &byRelease[at[0]][at[1]]
				v.Total += d.Total
				v.Installed += d.Installed
				v.Installing += d.Installing
				v.Failed += d.Failed
				v.Legacy = true
				// A rollout whose devices are all on the legacy path never leaves 'pending'
				// (only an MDM send activates it), so it is running while its legacy half
				// still has devices on the way (#424, 22 Oct 2026).
				if d.Status == "active" && d.Total > d.Installed+d.Failed && (v.Status == "pending" || v.Status == "complete") {
					v.Status = "active"
				}
			}
			continue
		}
		if !productMatches(d.ReleaseProduct) {
			continue
		}
		byRelease[d.ReleaseID] = append(byRelease[d.ReleaseID], legacyView(d))
	}
	// The pinned strip, after the fold so it carries both halves' counts.
	for _, rs := range byRelease {
		for _, v := range rs {
			if v.Status == "active" {
				active = append(active, v)
			}
		}
	}
	for id := range byRelease {
		sort.SliceStable(byRelease[id], func(i, j int) bool { return byRelease[id][i].CreatedAt.After(byRelease[id][j].CreatedAt) })
	}
	sort.SliceStable(active, func(i, j int) bool { return active[i].CreatedAt.After(active[j].CreatedAt) })
	return byRelease, active
}

func (h *Handler) ReleasesPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_ = h.db.CompleteSettledDeployments(ctx)
	rv, err := h.releaseVersionRows(r)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	filterProduct := r.URL.Query().Get("product")
	productMatches := func(p string) bool {
		if filterProduct == "" {
			return true
		}
		want, _ := product.Resolve(filterProduct)
		got, _ := product.Resolve(p)
		return want.Key == got.Key
	}

	// Every rollout, once: the pinned strip takes the active ones, the selected release
	// its own.
	deployments, _ := h.db.ListDeployments(ctx)
	deployments = visibleDeployments(h.role(r), deployments)
	legacy, _ := h.db.ListLegacyDeployments(ctx)
	byRelease, active := mergeRollouts(deployments, legacy, productMatches)

	// The selection: ?release=<id>, or ?build=<version> for a reported build, else the
	// newest release that is not a dev build.
	var sel *versionRow
	if id, err := strconv.Atoi(r.URL.Query().Get("release")); err == nil {
		for _, list := range [][]versionRow{rv.Active, rv.Hidden} {
			for i := range list {
				if list[i].ReleaseID != nil && *list[i].ReleaseID == id {
					sel = &list[i]
				}
			}
		}
	} else if b := r.URL.Query().Get("build"); b != "" {
		for i := range rv.Active {
			if rv.Active[i].Version == b {
				sel = &rv.Active[i]
			}
		}
	}
	if sel == nil {
		for i := range rv.Active {
			if rv.Active[i].Tracked && !rv.Active[i].IsDev {
				sel = &rv.Active[i]
				break
			}
		}
	}

	devCount := 0
	for _, v := range rv.Active {
		if v.Tracked && v.IsDev {
			devCount++
		}
	}
	data := map[string]any{
		"Title":          "Releases",
		"Versions":       rv.Active,
		"HiddenReleases": rv.Hidden,
		"FleetTotal":     rv.FleetTotal,
		"NotTracked":     rv.NotTracked,
		"DevCount":       devCount,
		"Active":         active,
		"Strip":          buildRoStrip(active),
		"RolloutCounts":  rolloutCounts(byRelease),
		"Products":       h.releaseProductFilters(ctx),
		"FilterProduct":  filterProduct,
		"Sel":            sel,
	}
	if sel != nil && sel.ReleaseID != nil {
		rel, err := h.db.GetRelease(ctx, *sel.ReleaseID)
		if err == nil {
			data["Rel"] = rel
			pkgs, _ := h.db.ListPackagesByRelease(ctx, rel.ID)
			data["Packages"] = pkgs
			canPush := false
			for _, p := range pkgs {
				if p.Status == "active" {
					canPush = true
				}
			}
			data["CanPush"] = canPush && rel.Status == "published"
			data["Rollouts"] = byRelease[rel.ID]
			// The release page's own actions, so the pane can offer them too: dev / everyone,
			// publish (warned when there is no full package), the flashing bundle, delete.
			hasFull := false
			for _, p := range pkgs {
				if p.Status == "active" && p.Type == "full" {
					hasFull = true
				}
			}
			data["HasFull"] = hasFull
			data["QFILPackages"], _ = h.db.ListQFILPackagesByRelease(ctx, rel.ID)
			data["DevicesCount"], _ = h.db.CountDevicesByVersion(ctx, rel.Version, rel.Product)

			// R2 (21 Oct 2026): the detail as a banner and cards.
			role := h.role(r)
			in := r2Inputs{Row: *sel, Rel: rel, ProductLabel: product.Label(rel.Product), Packages: pkgs,
				Rollouts: byRelease[rel.ID], FleetTotal: rv.FleetTotal, ShowCrashes: !h.access(r).hidesDevices(),
				CanRelease: role == "admin", CanOTA: roleCanOTA(role)}
			if n, err := h.db.ReleaseInstallsSince(ctx, rel.ID, 7); err == nil {
				in.InstallsWeek = n
			}
			if in.ShowCrashes {
				if c, err := h.db.BuildCrashCounts(ctx, rel.Version); err == nil {
					in.Crashes = c
				} else {
					log.Printf("[releases] crash counts: %v", err)
				}
			}
			data["R2"] = buildReleaseR2(in)
		}
	}
	h.render(w, r, "releases.html", data)
}

// rolloutCount is how many rollouts a release has, and how many are running.
type rolloutCount struct{ N, Active int }

// rolloutCounts keys the counts by version, which is what the list rows carry.
func rolloutCounts(byRelease map[int][]rolloutView) map[string]rolloutCount {
	out := map[string]rolloutCount{}
	for _, rs := range byRelease {
		for _, v := range rs {
			c := out[v.Version]
			c.N++
			if v.Status == "active" {
				c.Active++
			}
			out[v.Version] = c
		}
	}
	return out
}
