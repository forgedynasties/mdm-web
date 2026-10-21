package dashboard

import (
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
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
}

// Pending is what has not started or finished either way.
func (v rolloutView) Pending() int {
	n := v.Total - v.Installed - v.Installing - v.Failed
	if n < 0 {
		return 0
	}
	return n
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
		CreatedAt: d.CreatedAt, CreatedBy: d.CreatedBy, Total: d.DeviceTotal, Installed: d.DeviceInstalled + d.DeviceAwaiting,
		Installing: d.DeviceDownloading + d.DeviceInstalling, Failed: d.DeviceFailed}
	if d.Release != nil {
		v.Version = d.Release.Version
	}
	return v
}

func legacyView(d db.LegacyDeployment) rolloutView {
	return rolloutView{URL: fmt.Sprintf("/updates/legacy/deployments/%d", d.ID), ID: d.ID, Version: d.ReleaseVersion, Status: d.Status,
		CreatedAt: d.CreatedAt, CreatedBy: d.CreatedBy, Legacy: true, Total: d.Total, Installed: d.Installed, Failed: d.Failed}
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
	byRelease := map[int][]rolloutView{}
	var active []rolloutView
	for _, d := range deployments {
		if !productMatches(d.Product) {
			continue
		}
		v := deploymentView(d)
		byRelease[d.ReleaseID] = append(byRelease[d.ReleaseID], v)
		if d.Status == "active" {
			active = append(active, v)
		}
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
				v.Failed += d.Failed
				v.Legacy = true
			}
			continue
		}
		if !productMatches(d.ReleaseProduct) {
			continue
		}
		v := legacyView(d)
		byRelease[d.ReleaseID] = append(byRelease[d.ReleaseID], v)
		if d.Status == "active" {
			active = append(active, v)
		}
	}
	for id := range byRelease {
		sort.SliceStable(byRelease[id], func(i, j int) bool { return byRelease[id][i].CreatedAt.After(byRelease[id][j].CreatedAt) })
	}
	sort.SliceStable(active, func(i, j int) bool { return active[i].CreatedAt.After(active[j].CreatedAt) })

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
		"Title":         "Releases",
		"Versions":      rv.Active,
		"HiddenReleases": rv.Hidden,
		"FleetTotal":    rv.FleetTotal,
		"NotTracked":    rv.NotTracked,
		"DevCount":      devCount,
		"Active":        active,
		"RolloutCounts": rolloutCounts(byRelease),
		"Products":      h.releaseProductFilters(ctx),
		"FilterProduct": filterProduct,
		"Sel":           sel,
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
