package dashboard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	_ "image/gif"
	_ "image/jpeg"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"mdm/internal/db"
	"mdm/internal/ota"
	"mdm/internal/product"

	"github.com/google/uuid"
)

// ReleaseRename changes only a release's display name (not its changelog or other
// meta), so the fleet toolbar's inline rename can't blank the release notes.
func (h *Handler) ReleaseRename(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "Name required", http.StatusBadRequest)
		return
	}
	if err := h.db.SetReleaseName(r.Context(), id, name); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "release.rename", strconv.Itoa(id), name)
	localRedirect(w, r, "/releases")
}

// ── OTA Packages & Deployments ────────────────────────────────────────────────

// versionRow is one row of the unified releases-and-versions table: a release version,
// whether it's a tracked release (with lifecycle) or only seen on devices, and how many
// devices report it.
type versionRow struct {
	Version           string
	ReleaseID         *int
	Product           string // hardware product a tracked release targets ("" for untracked builds)
	Name              string
	Changelog         string     // release notes (tracked releases only) — shown as a snippet on the lean list
	Status            string     // "" when not tracked
	ReleasedAt        *time.Time // release date (tracked releases only) — the release's created_at, which also defines ordering
	Tracked           bool
	Hidden            bool
	DeviceCount       int
	PackageCount      int
	DeployCount       int
	Problems          db.ProblemSummary // open/blocker problem counts for the tracked release
	IsBranch          bool             // off-mainline branch build (shown nested under its parent in the list)
	IsDev             bool             // dev-only release — listed for devs/admins, invisible to everyone else
	ParentID          *int             // for a branch/derivative, the release id it forks from / matches
	ParentVersion     string           // for a branch, the release it forked from; for a derivative, the release it matches
	AdoptableParentID *int             // set when this is an UNTRACKED reported build matching a release's naming — one-click adopt as a branch of it
	Children          []versionRow     // branch builds + adoptable derivative builds forked off this release, shown indented beneath it
	SuggestedBranch   string           // next branch version to suggest for this release (version + -tN)
	QfilURL           string           // newest active QFIL flashing bundle URL ("" = none set)
	Merged            bool             // branch: has been merged onto main (terminal)
	MergedIntoVersion string           // branch: the mainline release it merged into
	MergedFromVersion string           // mainline node: the branch it absorbed on merge
	Crash             *releaseCrashTag // crash issues on this build (release issues), nil when none
}

// releaseCrashTag is the tag on a release row naming its crash issues: crashes that
// hit this build on several devices in the last two weeks (see ListCrashIssues).
type releaseCrashTag struct {
	Issues  int
	Devices int    // the widest issue's reach
	Apps    string // "pkg crash (34 devices), …" for the tooltip
	Warn    bool   // only ANRs: slow, not crashing
}

// releaseCrashTags groups the fleet's release issues by release id.
func (h *Handler) releaseCrashTags(ctx context.Context) map[int]*releaseCrashTag {
	issues, err := h.db.ListCrashIssues(ctx, 14, 500)
	if err != nil {
		return nil
	}
	out := map[int]*releaseCrashTag{}
	for _, c := range issues {
		if c.ReleaseID == 0 {
			continue
		}
		t := out[c.ReleaseID]
		if t == nil {
			t = &releaseCrashTag{Warn: true}
			out[c.ReleaseID] = t
		}
		t.Issues++
		if c.Devices > t.Devices {
			t.Devices = c.Devices
		}
		label, _ := crashKindBadge(c.Kind)
		if label != "ANR" {
			t.Warn = false
		}
		app := extractPackageName(c.Summary)
		if app == "" {
			app = c.Summary
		}
		if t.Apps != "" {
			t.Apps += ", "
		}
		t.Apps += fmt.Sprintf("%s %s (%d devices)", app, strings.ToLower(label), c.Devices)
	}
	return out
}

// latestQfilURL returns the newest active QFIL bundle URL for a release, or ""
// when none is set (so the releases list can grey out the download icon).
func (h *Handler) latestQfilURL(r *http.Request, releaseID int) string {
	if qpkgs, _ := h.db.ListQFILPackagesByRelease(r.Context(), releaseID); len(qpkgs) > 0 {
		return qpkgs[0].URL
	}
	return ""
}

// ReleaseSetDev turns a release's dev flag on or off. Off publishes it to every
// role; on takes it back out of sight for anyone but devs and admins.
func (h *Handler) ReleaseSetDev(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	dev := r.FormValue("dev") == "1"
	if err := h.db.SetReleaseDev(r.Context(), id, dev); err != nil {
		h.hxDoneToast(w, r, fmt.Sprintf("/releases/%d", id), "Could not change the release visibility", "error")
		return
	}
	h.audit(r, "release.set_dev", strconv.Itoa(id), map[bool]string{true: "dev", false: "everyone"}[dev])
	msg := "Released to everyone · it now shows on the Updates page for every role"
	if dev {
		msg = "Marked dev · only devs and admins can see it"
	}
	h.hxDoneToast(w, r, releaseBack(r, id), msg, "success")
}

// productLabels holds the model names learned from what devices report, keyed by
// product. Package-level because the template func map is built before any Handler
// exists; written by productFilters, read by the productLabel template func.
var productLabels atomic.Value // map[string]string

// productLabel is the template-facing label for a product key: a learned model name
// when we have one, else the catalog label (which falls back to the key itself).
func productLabel(key string) string {
	if n := adminProductName(key); n != "" {
		return n
	}
	if m, ok := productLabels.Load().(map[string]string); ok {
		if label := m[product.Normalize(key)]; label != "" {
			return label
		}
	}
	return product.Label(key)
}

// productFilters is the catalog plus every other product the fleet actually reports,
// so a device that is not our own hardware — a Sunmi D3 PRO, a stock Pixel running the
// DPC agent — can still be filtered for instead of hiding behind "All products".
// Catalog order first, then the newcomers by how many devices run them.
func (h *Handler) productFilters(ctx context.Context) []product.Product {
	out := product.All()
	seen := make(map[string]bool, len(out))
	for _, p := range out {
		seen[p.Key] = true
	}
	counts, err := h.db.FleetProducts(ctx)
	if err != nil {
		return out
	}
	learned := map[string]string{}
	for _, c := range counts {
		if label := modelLabel(c.Manufacturer, c.Model); label != "" && !product.IsKnown(c.Product) {
			learned[product.Normalize(c.Product)] = label
		}
	}
	productLabels.Store(learned)
	for _, c := range counts {
		key := product.Normalize(c.Product)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		p, _ := product.Resolve(c.Product)
		if label := modelLabel(c.Manufacturer, c.Model); label != "" {
			p.Label = label
		}
		out = append(out, p)
	}
	for i := range out {
		if n := adminProductName(out[i].Key); n != "" {
			out[i].Label = n
		}
	}
	return out
}

// releaseProductFilters is productFilters for everything release-shaped — Updates,
// Releases, rollouts, the push screen. A release is a firmware image for our own
// hardware; a DPC-managed device has no firmware we ship, so offering its product as
// a filter there only ever leads to an empty list.
func (h *Handler) releaseProductFilters(ctx context.Context) []product.Product {
	all := h.productFilters(ctx)
	out := all[:0:0]
	for _, p := range all {
		if p.Kind == product.KindAndroid {
			continue
		}
		out = append(out, p)
	}
	return out
}

// modelLabel turns what a device reports about itself into the name on the box:
// "SUNMI" + "D3 PRO" -> "Sunmi D3 PRO". Returns "" when the device said nothing,
// leaving the product key as the label.
func modelLabel(manufacturer, model string) string {
	man, mod := strings.TrimSpace(manufacturer), strings.TrimSpace(model)
	if mod == "" {
		return ""
	}
	if man == "" {
		return mod
	}
	// Manufacturers shout their name ("SUNMI", "SAMSUNG"); title-case it, and don't
	// repeat it when the model already carries it ("Pixel 8" from "Google").
	man = strings.ToUpper(man[:1]) + strings.ToLower(man[1:])
	if strings.HasPrefix(strings.ToLower(mod), strings.ToLower(man)) {
		return mod
	}
	return man + " " + mod
}

// roleSeesDev reports whether a role may see releases still marked dev. Everyone
// who works the fleet can: the flag keeps work in progress out of the DEFAULT view
// (the list folds dev releases away behind a toggle), not out of reach — so anyone
// with OTA rights can push one when that is what the job needs. Operator and up;
// viewers and support, who don't touch releases, and customers do not.
func roleSeesDev(role string) bool { return roleCanOperate(role) }

// visibleReleases drops dev-only releases for roles that may not see them.
func visibleReleases(role string, in []db.Release) []db.Release {
	if roleSeesDev(role) {
		return in
	}
	out := in[:0]
	for _, rel := range in {
		if !rel.IsDev {
			out = append(out, rel)
		}
	}
	return out
}

// visibleRail is visibleReleases for the compact release rail.
func visibleRail(role string, in []db.ReleaseRailItem) []db.ReleaseRailItem {
	if roleSeesDev(role) {
		return in
	}
	out := in[:0]
	for _, rel := range in {
		if !rel.IsDev {
			out = append(out, rel)
		}
	}
	return out
}

// visibleDeployments drops rollouts of dev-only releases for the same roles.
func visibleDeployments(role string, in []db.Update) []db.Update {
	if roleSeesDev(role) {
		return in
	}
	out := in[:0]
	for _, u := range in {
		if !u.ReleaseIsDev {
			out = append(out, u)
		}
	}
	return out
}

// releaseVersions is the release catalogue as /rollouts lists it: every tracked release
// and every version devices report without one (newest release first), the hidden ones
// apart, and how many devices the fleet has.
type releaseVersions struct {
	Active, Hidden        []versionRow
	Untracked             []string
	Tracked, NotTracked   int
	FleetTotal            int
}

// releaseVersionRows builds the catalogue (it was the body of the old Releases list).
func (h *Handler) releaseVersionRows(r *http.Request) (releaseVersions, error) {
	releases, err := h.db.ListReleases(r.Context())
	if err != nil {
		return releaseVersions{}, err
	}
	// Dev releases are invisible outside dev/admin: a build a device happens to
	// report still shows as an untracked version, which is honest — the fleet is
	// running it — but it carries none of the release's detail.
	if !roleSeesDev(h.role(r)) {
		kept := releases[:0]
		for _, rel := range releases {
			if !rel.IsDev {
				kept = append(kept, rel)
			}
		}
		releases = kept
	}
	// Merge tracked releases with the versions devices actually report, into one
	// version-centric table (fleet versions come pre-sorted by adoption desc).
	fleet, _ := h.db.GetFleetVersions(r.Context())
	hiddenVersions, _ := h.db.ListHiddenVersions(r.Context())
	problemsByRelease, _ := h.db.ProblemSummariesByRelease(r.Context())
	crashTags := h.releaseCrashTags(r.Context())
	// The list-row problem badge should reflect what the workspace board shows —
	// native PLUS carried-forward problems — so a build whose only open blockers are
	// inherited doesn't read "0 open" in the list.
	carriedByRelease, _ := h.db.CarriedProblemCountsByRelease(r.Context())
	badgeFor := func(id int) db.ProblemSummary {
		s := problemsByRelease[id]
		c := carriedByRelease[id]
		s.Open += c.Open
		s.Blockers += c.Blockers
		return s
	}
	relByVersion := make(map[string]db.Release, len(releases))
	relByID := make(map[int]db.Release, len(releases))
	for _, rel := range releases {
		relByVersion[rel.Version] = rel
		relByID[rel.ID] = rel
	}
	// Mainline release versions for naming-based branch matching (longest prefix wins), so
	// a reported build like "v2.0.3-t1" is recognised as a variant of release "v2.0.3".
	type mrel struct {
		id  int
		ver string
	}
	var mainline []mrel
	for _, rel := range releases {
		if !rel.IsBranch {
			mainline = append(mainline, mrel{rel.ID, rel.Version})
		}
	}
	bestBranchParent := func(build string) (*int, string) {
		bestLen := -1
		var bid int
		var bver string
		for _, m := range mainline {
			if strings.HasPrefix(build, m.ver+"-") && len(m.ver) > bestLen {
				bestLen, bid, bver = len(m.ver), m.id, m.ver
			}
		}
		if bestLen < 0 {
			return nil, ""
		}
		return &bid, bver
	}
	var active, hidden []versionRow
	var trackedCount, notTrackedCount int
	var untrackedBuilds []string // all untracked reported builds, for the New-branch picker
	seen := make(map[string]bool)
	childrenByParent := map[int][]versionRow{}
	// Optional per-product filter (?product=): a release/build is kept when its product
	// resolves to the selected one (empty/legacy -> t7). Empty filter shows everything.
	filterProduct := strings.TrimSpace(r.URL.Query().Get("product"))
	productMatches := func(p string) bool {
		if filterProduct == "" {
			return true
		}
		want, _ := product.Resolve(filterProduct)
		got, _ := product.Resolve(p)
		return want.Key == got.Key
	}
	addRow := func(row versionRow) {
		if !productMatches(row.Product) {
			return
		}
		// Branch builds and adoptable derivative builds hang off their parent — collected
		// by parent id and rendered indented beneath it in the list (no separate tab).
		if row.IsBranch || row.AdoptableParentID != nil {
			pid := 0
			if row.AdoptableParentID != nil {
				pid = *row.AdoptableParentID
			} else if row.ParentID != nil {
				pid = *row.ParentID
			}
			childrenByParent[pid] = append(childrenByParent[pid], row)
			return
		}
		if row.Hidden {
			hidden = append(hidden, row)
			return
		}
		if row.Tracked {
			trackedCount++
		} else {
			notTrackedCount++
		}
		active = append(active, row)
	}
	branchRow := func(row *versionRow, rel db.Release) {
		row.IsBranch = rel.IsBranch
		if rel.ParentReleaseID != nil {
			row.ParentID = rel.ParentReleaseID
			row.ParentVersion = relByID[*rel.ParentReleaseID].Version
		}
		// Merge lineage: a merged branch is terminal; a mainline node born from a merge
		// carries a back-reference to the branch it absorbed. Both drive list chips.
		if rel.MergedIntoReleaseID != nil {
			row.Merged = true
			row.MergedIntoVersion = relByID[*rel.MergedIntoReleaseID].Version
		}
		if rel.MergedFromReleaseID != nil {
			row.MergedFromVersion = relByID[*rel.MergedFromReleaseID].Version
		}
	}
	for _, fv := range fleet {
		seen[fv.Version] = true
		row := versionRow{Version: fv.Version, DeviceCount: fv.DeviceCount, Product: fv.Product}
		if rel, ok := relByVersion[fv.Version]; ok {
			id := rel.ID
			row.Tracked, row.ReleaseID, row.Name = true, &id, rel.Name
			row.Product = rel.Product
			row.Changelog = rel.Changelog
			row.Status, row.Hidden, row.IsDev = rel.Status, rel.Hidden, rel.IsDev
			ca := rel.CreatedAt
			row.ReleasedAt = &ca
			row.PackageCount, row.DeployCount = rel.PackageCount, rel.DeployCount
			branchRow(&row, rel)
			row.Problems = badgeFor(rel.ID)
			row.QfilURL = h.latestQfilURL(r, rel.ID)
			row.Crash = crashTags[rel.ID]
		} else {
			row.Hidden = hiddenVersions[fv.Version] // not-tracked versions dismissed by ops
			if !row.Hidden {
				untrackedBuilds = append(untrackedBuilds, fv.Version)
				// Auto-match by naming: an untracked reported build that looks like a variant
				// of a mainline release is shown under it for one-click adoption.
				if pid, pver := bestBranchParent(fv.Version); pid != nil {
					row.AdoptableParentID, row.ParentVersion = pid, pver
				}
			}
		}
		addRow(row)
	}
	// Tracked releases nobody is running yet (not in the fleet list).
	for _, rel := range releases {
		if seen[rel.Version] {
			continue
		}
		id := rel.ID
		ca := rel.CreatedAt
		row := versionRow{
			Version: rel.Version, Tracked: true, ReleaseID: &id, Name: rel.Name,
			Product:    rel.Product,
			Changelog:  rel.Changelog,
			ReleasedAt: &ca,
			Status:     rel.Status, Hidden: rel.Hidden, IsDev: rel.IsDev,
			PackageCount: rel.PackageCount, DeployCount: rel.DeployCount,
			Problems: badgeFor(rel.ID),
			QfilURL:  h.latestQfilURL(r, rel.ID),
			Crash:    crashTags[rel.ID],
		}
		branchRow(&row, rel)
		addRow(row)
	}
	// Order strictly by release date, newest first. Undated rows (reported builds
	// that aren't managed releases) have no release date and sort to the bottom.
	// The legacy manual drag order is no longer applied — the drag handle was
	// removed from the list and its version_order table is stale/unreachable.
	sort.SliceStable(active, func(i, j int) bool {
		ri, rj := active[i].ReleasedAt, active[j].ReleasedAt
		if ri == nil || rj == nil {
			return ri != nil // dated (ri!=nil) sorts before undated; two undated keep order
		}
		return ri.After(*rj)
	})
	// Attach each release's branch forks (+ adoptable derivative builds) so they render
	// nested beneath it, and suggest the next branch version (version + -tN).
	for i := range active {
		if active[i].ReleaseID == nil {
			continue
		}
		pid := *active[i].ReleaseID
		active[i].Children = childrenByParent[pid]
		delete(childrenByParent, pid)
		n := 0
		for _, c := range active[i].Children {
			if c.IsBranch {
				n++
			}
		}
		active[i].SuggestedBranch = fmt.Sprintf("%s-t%d", active[i].Version, n+1)
	}
	// Orphan forks (parent hidden or not shown) — surface standalone so they aren't lost.
	for _, kids := range childrenByParent {
		active = append(active, kids...)
	}
	role := h.role(r)
	// Hidden releases are admin-only housekeeping — don't surface them (or the
	// "N hidden" count) to operators/operators.
	if role != "admin" {
		hidden = nil
	}
	fleetTotal, _ := h.db.CountDevices(r.Context(), db.DeviceFilter{})
	return releaseVersions{Active: active, Hidden: hidden, Untracked: untrackedBuilds,
		Tracked: trackedCount, NotTracked: notTrackedCount, FleetTotal: fleetTotal}, nil
}

// ── Release lineage graph ─────────────────────────────────────────────────────
// A commit-style graph of the single main line: mainline releases form the spine
// (oldest→newest), each branch forks off its base node and, if merged, rejoins the
// main line at the successor it merged into. Coordinates are computed server-side and
// rendered as inline SVG by releases.html.

type relGraphNode struct {
	ID       int
	Label    string
	Sub      string
	IsBranch bool
	Merged   bool
	Fill     string
	X, Y, R  int
}

type relGraphEdge struct {
	Path string // SVG path 'd'
	Kind string // main | fork | merge
}

type relGraph struct {
	Nodes []relGraphNode
	Edges []relGraphEdge
	W, H  int
}

func statusFill(status string) string {
	switch status {
	case "published":
		return "#1c7d50"
	case "draft":
		return "#9a6512"
	default:
		return "#9aa0aa"
	}
}

// buildReleaseGraph lays out the release lineage. Mainline nodes are spaced evenly along
// the spine; branches are packed into stacked lanes above it (a greedy interval colouring
// so non-overlapping forks share a lane), with a fork edge down to their base and a merge
// edge across to the release they merged into.
func buildReleaseGraph(rels []db.Release) relGraph {
	var main, branch []db.Release
	for _, r := range rels {
		if r.Hidden {
			continue
		}
		if r.IsBranch {
			branch = append(branch, r)
		} else {
			main = append(main, r)
		}
	}
	if len(main)+len(branch) == 0 {
		return relGraph{}
	}
	sort.Slice(main, func(i, j int) bool { return main[i].CreatedAt.Before(main[j].CreatedAt) })
	sort.Slice(branch, func(i, j int) bool { return branch[i].CreatedAt.Before(branch[j].CreatedAt) })

	const stepX, marginX, laneStep, topPad, botPad, nodeR = 175, 72, 46, 30, 56, 7
	mainX := map[int]int{}
	for i, r := range main {
		mainX[r.ID] = marginX + i*stepX
	}
	baseXof := func(b db.Release) int {
		if b.ParentReleaseID != nil {
			if x, ok := mainX[*b.ParentReleaseID]; ok {
				return x
			}
		}
		return marginX
	}

	branchX := map[int]int{}
	laneOf := map[int]int{}
	var laneEnds []int // rightmost x used on each lane so far
	for _, b := range branch {
		baseX := baseXof(b)
		bx, x2 := baseX+stepX/2, baseX+stepX/2+nodeR
		if b.MergedIntoReleaseID != nil {
			if mx, ok := mainX[*b.MergedIntoReleaseID]; ok {
				bx, x2 = (baseX+mx)/2, mx
			}
		}
		lane := -1
		for l, end := range laneEnds {
			if end < baseX-24 {
				lane, laneEnds[l] = l, x2
				break
			}
		}
		if lane == -1 {
			lane = len(laneEnds)
			laneEnds = append(laneEnds, x2)
		}
		branchX[b.ID], laneOf[b.ID] = bx, lane
	}

	laneCount := len(laneEnds)
	spineY := topPad + laneCount*laneStep
	laneY := func(l int) int { return spineY - (l+1)*laneStep }
	// Width spans the rightmost node (a branch forking off the last main node can sit past
	// the spine) plus a margin so its label isn't clipped.
	maxX := marginX
	for _, x := range mainX {
		if x > maxX {
			maxX = x
		}
	}
	for _, x := range branchX {
		if x > maxX {
			maxX = x
		}
	}
	g := relGraph{W: maxX + marginX, H: spineY + botPad}

	// Edges first so nodes paint on top.
	for i := 1; i < len(main); i++ {
		x0, x1 := mainX[main[i-1].ID], mainX[main[i].ID]
		g.Edges = append(g.Edges, relGraphEdge{Kind: "main", Path: fmt.Sprintf("M%d %d L%d %d", x0, spineY, x1, spineY)})
	}
	for _, b := range branch {
		bx, by := branchX[b.ID], laneY(laneOf[b.ID])
		baseX := baseXof(b)
		ymid := (spineY + by) / 2
		g.Edges = append(g.Edges, relGraphEdge{Kind: "fork", Path: fmt.Sprintf("M%d %d C%d %d %d %d %d %d", baseX, spineY, baseX, ymid, bx, ymid, bx, by)})
		if b.MergedIntoReleaseID != nil {
			if mx, ok := mainX[*b.MergedIntoReleaseID]; ok {
				g.Edges = append(g.Edges, relGraphEdge{Kind: "merge", Path: fmt.Sprintf("M%d %d C%d %d %d %d %d %d", bx, by, bx, ymid, mx, ymid, mx, spineY)})
			}
		}
	}
	for _, r := range main {
		g.Nodes = append(g.Nodes, relGraphNode{ID: r.ID, Label: r.Version, Sub: r.Status, Fill: statusFill(r.Status), X: mainX[r.ID], Y: spineY, R: nodeR})
	}
	for _, b := range branch {
		g.Nodes = append(g.Nodes, relGraphNode{ID: b.ID, Label: b.Version, Sub: "branch", IsBranch: true, Merged: b.MergedIntoReleaseID != nil, X: branchX[b.ID], Y: laneY(laneOf[b.ID]), R: nodeR - 1})
	}
	return g
}

// ReleaseTrack starts tracking a device-reported version: it creates (or finds) a draft
// release for that version and drops the user on its detail page to add changelog/packages.
func (h *Handler) ReleaseTrack(w http.ResponseWriter, r *http.Request) {
	version := strings.TrimSpace(r.FormValue("version"))
	if version == "" {
		http.Redirect(w, r, "/releases", http.StatusSeeOther)
		return
	}
	// A version tracked from the live fleet inherits the product of the devices on it.
	product, _ := h.db.ProductForVersion(r.Context(), version)
	rel, err := h.db.GetOrCreateRelease(r.Context(), version, product)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "release.track", version, "")
	http.Redirect(w, r, fmt.Sprintf("/releases/%d", rel.ID), http.StatusSeeOther)
}

// ReorderVersions saves the drag-and-drop order of the releases list (JSON body
// {"versions": [...]} in display order). Returns 204 on success (called via fetch).
func (h *Handler) ReorderVersions(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Versions []string `json:"versions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	if err := h.db.SetVersionOrder(r.Context(), body.Versions); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "release.reorder", "", strconv.Itoa(len(body.Versions)))
	w.WriteHeader(http.StatusNoContent)
}

// VersionHide dismisses a not-tracked device-reported version from the releases list
// (and from the "Add as release" options). VersionUnhide brings it back.
func (h *Handler) VersionHide(w http.ResponseWriter, r *http.Request) {
	version := strings.TrimSpace(r.FormValue("version"))
	if version != "" {
		if err := h.db.HideVersion(r.Context(), version); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		h.audit(r, "version.hide", version, "")
	}
	http.Redirect(w, r, "/releases", http.StatusSeeOther)
}

func (h *Handler) VersionUnhide(w http.ResponseWriter, r *http.Request) {
	version := strings.TrimSpace(r.FormValue("version"))
	if version != "" {
		if err := h.db.UnhideVersion(r.Context(), version); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		h.audit(r, "version.unhide", version, "")
	}
	http.Redirect(w, r, "/releases", http.StatusSeeOther)
}

// ReleaseSetHidden hides/unhides a release from the main list (irrelevant releases).
func (h *Handler) ReleaseSetHidden(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	hidden := r.FormValue("hidden") == "1"
	if err := h.db.SetReleaseHidden(r.Context(), id, hidden); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	state := "unhidden"
	if hidden {
		state = "hidden"
	}
	h.audit(r, "release.hide", strconv.Itoa(id), state)
	http.Redirect(w, r, "/releases", http.StatusSeeOther)
}

// ReleaseDelete hard-deletes a release (cascades to its packages + deployments).
func (h *Handler) ReleaseDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	// A release that devices are still running can't be deleted — hide it instead
	// (keeps the changelog), or move those devices to another build first.
	if rel, err := h.db.GetRelease(r.Context(), id); err == nil {
		if n, err := h.db.CountDevicesByVersion(r.Context(), rel.Version, rel.Product); err == nil && n > 0 {
			http.Error(w, fmt.Sprintf("Cannot delete: %d device(s) are still running %s. Hide it instead (keeps the changelog), or move those devices to another build first.", n, rel.Version), http.StatusConflict)
			return
		}
	}
	if err := h.db.DeleteRelease(r.Context(), id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "release.delete", strconv.Itoa(id), "")
	http.Redirect(w, r, "/releases", http.StatusSeeOther)
}

// createPackageFromForm parses the package form, decodes the base64 update_url
// (WAF bypass), validates, and creates the package — auto-creating/linking its
// release. When forcedTargetBuild is non-empty (adding to an existing release)
// it overrides the form's target build. Returns the created package or false
// after having written an error response.
func (h *Handler) createPackageFromForm(w http.ResponseWriter, r *http.Request, forcedTargetBuild, forcedProduct string) (*db.OTAPackage, bool) {
	r.ParseForm()

	typ := r.FormValue("type")
	if typ != "full" && typ != "incremental" {
		typ = "full"
	}
	targetBuildID := strings.TrimSpace(r.FormValue("target_build_id"))
	if forcedTargetBuild != "" {
		targetBuildID = forcedTargetBuild
	}
	sourceBuildID := strings.TrimSpace(r.FormValue("source_build_id"))
	updateURL := strings.TrimSpace(r.FormValue("update_url"))
	// The dashboard sends update_url as URL-safe base64 (update_url_b64) so an
	// upstream WAF doesn't see the raw URL — an internal OTA host like
	// http://10.0.0.5:3001/update.zip otherwise trips managed SSRF/RFI rules and
	// the ALB rejects the POST with a 403 before it reaches us. Plain update_url
	// stays as a fallback for API callers that don't encode.
	if b64 := strings.TrimSpace(r.FormValue("update_url_b64")); b64 != "" {
		dec, err := base64.RawURLEncoding.DecodeString(b64)
		if err != nil {
			http.Error(w, "invalid update_url encoding", http.StatusBadRequest)
			return nil, false
		}
		updateURL = strings.TrimSpace(string(dec))
	}
	changelog := strings.TrimSpace(r.FormValue("changelog"))
	// Product tags a NEWLY-created release. When adding a package to an EXISTING release
	// the caller forces the release's own product — otherwise an empty form product would
	// resolve to t7 in CreateOTAPackage and silently attach the package to (or create) a
	// DIFFERENT t7 release of the same version instead of this one.
	product := strings.TrimSpace(r.FormValue("product"))
	if forcedProduct != "" {
		product = forcedProduct
	}

	if targetBuildID == "" || updateURL == "" {
		http.Error(w, "target_build_id and update_url are required", http.StatusBadRequest)
		return nil, false
	}
	if typ == "incremental" && sourceBuildID == "" {
		http.Error(w, "source_build_id is required for incremental updates", http.StatusBadRequest)
		return nil, false
	}

	pkg, err := h.db.CreateOTAPackage(r.Context(), typ, targetBuildID, sourceBuildID, updateURL, changelog, product, time.Now().UTC())
	if err != nil {
		http.Error(w, "Internal error: "+err.Error(), http.StatusInternalServerError)
		return nil, false
	}
	// Read the wipe flag from the zip itself; the form's "wipes data" box is a claim too.
	pkg.Wipe = ota.RecordWipe(r.Context(), h.db, pkg.ID, updateURL, r.FormValue("wipe") == "1")
	return pkg, true
}

// ReleaseCreate creates an empty release identified by its build id (the unique
// version). Packages — the full image and any incrementals — are added
// afterward on the release page.
func (h *Handler) ReleaseCreate(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	version := strings.TrimSpace(r.FormValue("version"))
	if version == "" {
		http.Error(w, "release build id is required", http.StatusBadRequest)
		return
	}
	product := strings.TrimSpace(r.FormValue("product"))
	rel, err := h.db.GetOrCreateRelease(r.Context(), version, product)
	if err != nil {
		http.Error(w, "Internal error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	changelog := strings.TrimSpace(r.FormValue("changelog"))
	if name != "" || changelog != "" {
		_ = h.db.SetReleaseMeta(r.Context(), rel.ID, name, changelog)
	}
	// New releases start dev (the form's checkbox is checked by default), so work in
	// progress is invisible to everyone but devs and admins until it is turned off.
	_ = h.db.SetReleaseDev(r.Context(), rel.ID, r.FormValue("is_dev") == "1")
	h.audit(r, "release.create", version, "")
	http.Redirect(w, r, fmt.Sprintf("/releases/%d", rel.ID), http.StatusSeeOther)
}

// ReleaseAddPackage adds another package (e.g. an incremental) to an existing
// release; the target build is fixed to the release's version.
func (h *Handler) ReleaseAddPackage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	rel, err := h.db.GetRelease(r.Context(), id)
	if err != nil {
		http.Error(w, "Release not found", http.StatusNotFound)
		return
	}
	// A release may hold exactly one full image (the baseline); additional
	// packages must be incrementals. Reject a second full server-side — the UI
	// also greys out the Full button once one exists.
	if r.FormValue("type") == "full" {
		pkgs, _ := h.db.ListPackagesByRelease(r.Context(), id)
		for _, p := range pkgs {
			if p.Type == "full" && p.Status == "active" {
				http.Error(w, "This release already has a full package. Add an incremental instead.", http.StatusBadRequest)
				return
			}
		}
	}
	if _, ok := h.createPackageFromForm(w, r, rel.Version, rel.Product); !ok {
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/releases/%d", id), http.StatusSeeOther)
}

// packageURLFromForm reads the OTA package URL from the add/inspect form, honoring the
// base64 (WAF-bypass) field the dashboard sends; falls back to the plain field.
func packageURLFromForm(r *http.Request) string {
	if b64 := strings.TrimSpace(r.FormValue("update_url_b64")); b64 != "" {
		if dec, err := base64.RawURLEncoding.DecodeString(b64); err == nil {
			return strings.TrimSpace(string(dec))
		}
	}
	return strings.TrimSpace(r.FormValue("update_url"))
}

// PackageInspect reads an OTA package's metadata straight from its URL (HTTP range
// requests — no multi-GB download) so the Add-package composer can auto-fill type and
// source/target build. Admin-only; returns JSON the composer consumes.
func (h *Handler) PackageInspect(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	if _, err := h.db.GetRelease(r.Context(), id); err != nil {
		http.Error(w, "Release not found", http.StatusNotFound)
		return
	}
	r.ParseForm()
	w.Header().Set("Content-Type", "application/json")
	url := packageURLFromForm(r)
	if url == "" {
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "Enter a package URL first."})
		return
	}
	m, err := ota.Inspect(r.Context(), url)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "Couldn't read the package: " + err.Error()})
		return
	}
	typ := "full"
	if m.IsIncremental {
		typ = "incremental"
	}
	// Deliberately NOT surfaced: any build id from the fingerprint. The device
	// fingerprint (and ro.build.id) is held constant for Play Integrity, so it doesn't
	// identify the version — reporting it would be misleading. We return what IS
	// meaningful: full vs incremental, the target device, and size/reachability.
	json.NewEncoder(w).Encode(map[string]any{
		"ok":             true,
		"size_bytes":     m.SizeBytes,
		"ota_type":       m.OTAType,
		"type":           typ,
		"is_incremental": m.IsIncremental,
		"device":         m.Device,
	})
}

// ReleaseCrashDelete removes a crash from a release's auto-detected crash list (admin
// cleanup of a noisy/irrelevant crash) and clears the device's crash alert, so it also
// disappears from the Alerts page.
func (h *Handler) ReleaseCrashDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	eid, err := uuid.Parse(r.PathValue("eid"))
	if err != nil {
		http.Error(w, "Invalid crash ID", http.StatusBadRequest)
		return
	}
	deviceID, ok, err := h.db.DeleteCrashEvent(r.Context(), eid)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	if ok {
		if err := h.db.DeleteDeviceCrashAlert(r.Context(), deviceID); err == nil {
			h.hub.PublishAlertUpdate() // refresh the Alerts page live
		}
	}
	h.audit(r, "release.crash.delete", strconv.Itoa(id), eid.String())
	http.Redirect(w, r, fmt.Sprintf("/releases/%d/crashes", id), http.StatusSeeOther)
}

// ReleaseCrashes is the full, paginated list of a release's auto-detected crashes,
// collapsed into signature groups (busiest first). Reached from the workspace's "Show
// all …" link when there are more crash groups than the workspace previews.
func (h *Handler) ReleaseCrashes(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	rel, err := h.db.GetRelease(r.Context(), id)
	if err != nil {
		http.Error(w, "Release not found", http.StatusNotFound)
		return
	}
	const pageSize = 25
	page := 1
	if p, e := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("page"))); e == nil && p > 0 {
		page = p
	}
	groups, total, _ := h.db.CrashGroupsOnBuild(r.Context(), rel.Version, pageSize, (page-1)*pageSize)
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages < 1 {
		totalPages = 1
	}
	// An out-of-range page (stale link) returns no rows and total 0; clamp and re-query so
	// the pager and count stay honest.
	if len(groups) == 0 && page > 1 {
		page = 1
		groups, total, _ = h.db.CrashGroupsOnBuild(r.Context(), rel.Version, pageSize, 0)
		totalPages = (total + pageSize - 1) / pageSize
		if totalPages < 1 {
			totalPages = 1
		}
	}
	h.render(w, r, "release_crashes.html", map[string]any{
		"Title":       "Crashes · " + rel.Version,
		"Release":     rel,
		"CrashGroups": groups,
		"Page":        page,
		"Total":       total,
		"TotalPages":  totalPages,
	})
}

// ReleaseCrashGroupDelete removes every crash matching a group's kind + signature on this
// build (admin cleanup of a noisy crash class) and clears each affected device's crash
// alert, so the group also disappears from the Alerts page.
func (h *Handler) ReleaseCrashGroupDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	rel, err := h.db.GetRelease(r.Context(), id)
	if err != nil {
		http.Error(w, "Release not found", http.StatusNotFound)
		return
	}
	r.ParseForm()
	kind := strings.TrimSpace(r.FormValue("kind"))
	sig := strings.TrimSpace(r.FormValue("signature"))
	if kind == "" || sig == "" {
		http.Error(w, "kind and signature are required", http.StatusBadRequest)
		return
	}
	devIDs, err := h.db.DeleteCrashGroup(r.Context(), rel.Version, kind, sig)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	for _, dev := range devIDs {
		_ = h.db.DeleteDeviceCrashAlert(r.Context(), dev)
	}
	if len(devIDs) > 0 {
		h.hub.PublishAlertUpdate() // refresh the Alerts page live
	}
	h.audit(r, "release.crash.group.delete", strconv.Itoa(id), kind+" "+sig)
	redirect := strings.TrimSpace(r.FormValue("redirect"))
	if !strings.HasPrefix(redirect, "/releases") {
		redirect = fmt.Sprintf("/releases/%d/crashes", id)
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

// ReleaseAddQFIL attaches a QFIL flashing bundle to a release. Admin/dev only
// (see the requireAdmin route guard); the test team reads the resulting list on
// the release page. The bundle is referenced by an external URL like an OTA
// package — the URL may be base64-encoded (qfil_url_b64) to slip past an upstream
// WAF, mirroring the OTA package form.
func (h *Handler) ReleaseAddQFIL(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	if _, err := h.db.GetRelease(r.Context(), id); err != nil {
		http.Error(w, "Release not found", http.StatusNotFound)
		return
	}
	r.ParseForm()
	label := strings.TrimSpace(r.FormValue("label"))
	notes := strings.TrimSpace(r.FormValue("notes"))
	url := strings.TrimSpace(r.FormValue("qfil_url"))
	if b64 := strings.TrimSpace(r.FormValue("qfil_url_b64")); b64 != "" {
		dec, err := base64.RawURLEncoding.DecodeString(b64)
		if err != nil {
			http.Error(w, "invalid qfil_url encoding", http.StatusBadRequest)
			return
		}
		url = strings.TrimSpace(string(dec))
	}
	if url == "" {
		http.Error(w, "qfil_url is required", http.StatusBadRequest)
		return
	}
	// A release carries a single QFIL bundle — adding one replaces any existing.
	if err := h.db.DeleteQFILPackagesByRelease(r.Context(), id); err != nil {
		http.Error(w, "Internal error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := h.db.CreateQFILPackage(r.Context(), id, label, url, notes, h.currentUsername(r)); err != nil {
		http.Error(w, "Internal error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.audit(r, "release.qfil.add", strconv.Itoa(id), label)
	// Return to wherever the edit came from (the list uses redirect=/releases);
	// only same-app release paths are honoured, defaulting to the release page.
	redirect := strings.TrimSpace(r.FormValue("redirect"))
	if !strings.HasPrefix(redirect, "/releases") {
		redirect = fmt.Sprintf("/releases/%d", id)
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

// ReleaseDeleteQFIL removes a QFIL bundle from a release. Admin/dev only.
func (h *Handler) ReleaseDeleteQFIL(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	qid, err := strconv.Atoi(r.PathValue("qid"))
	if err != nil {
		http.Error(w, "Invalid QFIL ID", http.StatusBadRequest)
		return
	}
	if err := h.db.DeleteQFILPackage(r.Context(), qid); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "release.qfil.delete", strconv.Itoa(id), strconv.Itoa(qid))
	http.Redirect(w, r, fmt.Sprintf("/releases/%d", id), http.StatusSeeOther)
}

// ReleaseDetail serves /releases/{id}, the release's own page (release_workspace.html):
// version, product, changelog and packages (tab from ?tab=, default overview).
func (h *Handler) ReleaseDetail(w http.ResponseWriter, r *http.Request) {
	h.renderReleaseWorkspace(w, r, r.URL.Query().Get("tab"))
}

func (h *Handler) renderReleaseWorkspace(w http.ResponseWriter, r *http.Request, tab string) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	rel, err := h.db.GetRelease(r.Context(), id)
	if err != nil {
		http.Error(w, "Release not found", http.StatusNotFound)
		return
	}
	if rel.IsDev && !roleSeesDev(h.role(r)) {
		http.Error(w, "Release not found", http.StatusNotFound)
		return
	}
	// Live refresh: ?partial=problems returns the OOB fragments that swap the problem
	// lists and badges in place — so another user's change lands here too.
	if r.URL.Query().Get("partial") == "problems" {
		h.writeReleaseProblemsResponse(w, r, id)
		return
	}
	h.render(w, r, "release_workspace.html", h.releaseWorkspaceData(r, rel, tab))
}

// releaseWorkspaceData assembles the release page's data. It builds on
// releaseProblemsData (Release/Role/Problems/ProblemSummary/CanReport/CanAct) and adds
// packages, deployments, crashes and rollout facts.
func (h *Handler) releaseWorkspaceData(r *http.Request, rel *db.Release, tab string) map[string]any {
	ctx := r.Context()
	data := h.releaseProblemsData(r, rel)
	role, _ := data["Role"].(string)

	packages, _ := h.db.ListPackagesByRelease(ctx, rel.ID)
	qfilPackages, _ := h.db.ListQFILPackagesByRelease(ctx, rel.ID)
	deployments, _ := h.db.ListDeploymentsByRelease(ctx, rel.ID)
	// Adoption count only — links out to the Fleet page (filtered by this build) rather
	// than embedding the device roster.
	devicesCount, _ := h.db.CountDevicesByVersion(ctx, rel.Version, rel.Product)
	// Tracked releases back the incremental package's "From build" picker (its source).
	sourceReleases, _ := h.db.ListReleases(ctx)
	// Top crash groups for the workspace; crashTotal drives the "Show all …" link to the
	// full paginated /releases/{id}/crashes page.
	crashGroups, crashTotal, _ := h.db.CrashGroupsOnBuild(ctx, rel.Version, 5, 0)
	devs, _ := h.db.ListDevices(ctx, db.DeviceFilter{BuildID: rel.Version}, 0, 500, "serial", "asc")
	// Crash samples name fleet-wide serials and carry traces; the device list is the
	// build's whole roster. Someone with a visibility limit gets their devices only.
	if acc := h.access(r); acc.hidesDevices() {
		crashGroups, crashTotal = nil, 0
		devs = acc.keepVisible(devs)
	}

	// hasFull gates the "Add full package" form; canPush gates the deploy CTA. An
	// incremental-only release is still pushable — the per-device resolver matches each
	// incremental to devices on its source build.
	hasFull, canPush := false, false
	for _, p := range packages {
		if p.Status != "active" {
			continue
		}
		canPush = true
		if p.Type == "full" {
			hasFull = true
		}
	}

	switch tab {
	case "overview", "problems", "packages", "rollout":
		// valid
	default:
		tab = "overview"
	}
	// Non-admins have no Packages tab; fall back so a stale deep-link isn't a blank page.
	if tab == "packages" && role != "admin" {
		tab = "overview"
	}
	// Branch context: a branch build shows a banner linking to its parent. Branch builds
	// themselves are created / listed on the main /releases page (nested under the parent),
	// not as a workspace tab.
	data["IsBranch"] = rel.IsBranch
	if rel.IsBranch && rel.ParentReleaseID != nil {
		if parent, err := h.db.GetRelease(ctx, *rel.ParentReleaseID); err == nil {
			data["Parent"] = parent
		}
	}
	title := "Release " + rel.Version
	if tab == "problems" {
		title = "Problems · " + rel.Version
	}

	data["Tab"] = tab
	data["Title"] = title
	data["Packages"] = packages
	data["QFILPackages"] = qfilPackages
	data["CanManageQFIL"] = role == "admin"
	data["Deployments"] = deployments
	data["HasFull"] = hasFull
	data["CanPush"] = canPush
	data["DevicesCount"] = devicesCount
	data["SourceReleases"] = sourceReleases
	data["CrashGroups"] = crashGroups
	data["CrashGroupTotal"] = crashTotal
	data["DevicesOnVersion"] = devs
	return data
}

// ReleaseProblemCreate files a new problem report against a release. Any operator
// or operator (canOperate) can report; the build is snapshotted from the release.
func (h *Handler) ReleaseProblemCreate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	rel, err := h.db.GetRelease(r.Context(), id)
	if err != nil {
		http.Error(w, "Release not found", http.StatusNotFound)
		return
	}
	r.ParseForm()
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		if hxReq(r) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Redirect(w, r, "/releases/"+strconv.Itoa(id)+"#problems", http.StatusFound)
		return
	}
	p := db.ReleaseProblem{
		ReleaseID:   id,
		BuildID:     rel.Version,
		Title:       title,
		Description: strings.TrimSpace(r.FormValue("description")),
		Severity:    r.FormValue("severity"),
		ReportedBy:  h.currentUsername(r),
	}
	// Device is picked by serial.
	if serial := strings.TrimSpace(r.FormValue("device_serial")); serial != "" {
		if dev, err := h.db.GetDevice(r.Context(), serial); err == nil {
			p.DeviceID = &dev.ID
			if p.BuildID == "" {
				p.BuildID = dev.BuildID
			}
		}
	}
	if _, err := h.db.CreateReleaseProblem(r.Context(), p); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "release.problem.create", rel.Version, fmt.Sprintf("severity=%s, title=%s", p.Severity, title))
	h.hub.PublishProblemUpdate() // live-refresh the hub board + any open workspace
	if hxReq(r) {
		h.writeReleaseProblemsResponse(w, r, id)
		return
	}
	http.Redirect(w, r, "/releases/"+strconv.Itoa(id)+"#problems", http.StatusFound)
}

// ReleaseProblemUpdate changes a problem's status (and optionally severity).
func (h *Handler) ReleaseProblemUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	pid, err := uuid.Parse(r.PathValue("pid"))
	if err != nil {
		http.Error(w, "Invalid problem ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	status := r.FormValue("status")
	// Status transitions record the fix/verify trail against THIS release (the build the
	// user is looking at), so a bug's "fixed in / verified in" is captured as it moves
	// through the release train. Severity-only edits still go through UpdateReleaseProblem.
	var uerr error
	switch status {
	case "fixed":
		uerr = h.db.MarkProblemFixedIn(r.Context(), pid, id)
	case "verified":
		uerr = h.db.VerifyProblem(r.Context(), pid, id)
	case "open":
		uerr = h.db.MarkProblemFixedIn(r.Context(), pid, 0) // reopen: clears the fix/verify trail
	default: // wontfix, or a severity-only change
		uerr = h.db.UpdateReleaseProblem(r.Context(), pid, status, r.FormValue("severity"))
	}
	if uerr != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	h.audit(r, "release.problem.update", strconv.Itoa(id), fmt.Sprintf("problem=%s, status=%s", pid, status))
	h.hub.PublishProblemUpdate()
	if hxReq(r) {
		h.writeReleaseProblemsResponse(w, r, id)
		return
	}
	http.Redirect(w, r, "/releases/"+strconv.Itoa(id)+"#problems", http.StatusFound)
}

// ReleaseProblemDelete removes a problem (admin only).
func (h *Handler) ReleaseProblemDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	pid, err := uuid.Parse(r.PathValue("pid"))
	if err != nil {
		http.Error(w, "Invalid problem ID", http.StatusBadRequest)
		return
	}
	if err := h.db.DeleteReleaseProblem(r.Context(), pid); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.hub.PublishProblemUpdate()
	if hxReq(r) {
		h.writeReleaseProblemsResponse(w, r, id)
		return
	}
	http.Redirect(w, r, "/releases/"+strconv.Itoa(id)+"#problems", http.StatusFound)
}

// ReleasePublish moves a release to published (deployable).
func (h *Handler) ReleasePublish(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	if err := h.db.SetReleaseStatus(r.Context(), id, "published"); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "release.publish", strconv.Itoa(id), "")
	http.Redirect(w, r, releaseBack(r, id), http.StatusSeeOther)
}

// releaseBack is where a release action returns to: the form's redirect when it is a
// release path (the Releases pane sends /releases?release=N), else the release's page.
func releaseBack(r *http.Request, id int) string {
	if v := strings.TrimSpace(r.FormValue("redirect")); strings.HasPrefix(v, "/releases") && !strings.HasPrefix(v, "//") {
		return v
	}
	return fmt.Sprintf("/releases/%d", id)
}

// ReleaseCreateBranch forks an off-mainline branch build from a release (admin/dev) — a
// temporary test build that stays off the main path (see CreateBranchRelease). Redirects
// to the new branch's workspace.
func (h *Handler) ReleaseCreateBranch(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	parent, err := h.db.GetRelease(r.Context(), id)
	if err != nil {
		http.Error(w, "Release not found", http.StatusNotFound)
		return
	}
	if parent.IsBranch {
		http.Error(w, "Cannot branch from a branch build", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	version := strings.TrimSpace(r.FormValue("version"))
	if version == "" {
		http.Redirect(w, r, fmt.Sprintf("/releases/%d?tab=branches", id), http.StatusFound)
		return
	}
	// Seed the branch with the parent's changelog so it starts from the same baseline.
	newID, err := h.db.CreateBranchRelease(r.Context(), id, version, strings.TrimSpace(r.FormValue("name")), parent.Changelog)
	if err != nil {
		http.Error(w, "Could not create branch — that version may already exist.", http.StatusBadRequest)
		return
	}
	h.audit(r, "release.branch.create", version, fmt.Sprintf("from=%s", parent.Version))
	http.Redirect(w, r, fmt.Sprintf("/releases/%d", newID), http.StatusSeeOther)
}

// ReleaseMerge merges a branch onto the main line as a new draft mainline release at the
// given version. The new release starts as a draft so the real v-next build is attached
// via the normal release-first flow.
func (h *Handler) ReleaseMerge(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	branch, err := h.db.GetRelease(r.Context(), id)
	if err != nil {
		http.Error(w, "Release not found", http.StatusNotFound)
		return
	}
	if !branch.IsBranch {
		http.Error(w, "Only a branch build can be merged to main.", http.StatusBadRequest)
		return
	}
	if branch.MergedIntoReleaseID != nil {
		http.Error(w, "This branch has already been merged.", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	version := strings.TrimSpace(r.FormValue("version"))
	if version == "" {
		http.Redirect(w, r, fmt.Sprintf("/releases/%d", id), http.StatusFound)
		return
	}
	changelog := strings.TrimSpace(r.FormValue("changelog"))
	if changelog == "" {
		changelog = branch.Changelog // start the mainline release from the branch's notes
	}
	carryBuild := r.FormValue("carry_build") == "on"
	newID, err := h.db.MergeBranch(r.Context(), id, branch.Version, version, strings.TrimSpace(r.FormValue("name")), changelog, h.currentUsername(r), carryBuild)
	if err != nil {
		http.Error(w, "Could not merge — that version may already exist.", http.StatusBadRequest)
		return
	}
	h.audit(r, "release.merge", version, fmt.Sprintf("from=%s", branch.Version))
	h.hub.PublishProblemUpdate() // refresh any open releases list (new node + branch closed)
	http.Redirect(w, r, fmt.Sprintf("/releases/%d", newID), http.StatusSeeOther)
}

// releaseProblemsData gathers the problems state that the release page and its htmx
// OOB fragments render from. Keys match what the rd-* partials expect.
func (h *Handler) releaseProblemsData(r *http.Request, rel *db.Release) map[string]any {
	ctx := r.Context()
	problems, _ := h.db.ListReleaseProblems(ctx, rel.ID)
	ps, _ := h.db.ReleaseProblemSummary(ctx, rel.ID)
	role := h.role(r)
	// Bugs that ride the release train onto this build (reported on an earlier build and
	// not yet verified fixed, plus any claimed fixed in this build awaiting verification).
	// Carry-forward is a super-admin concern — operators don't see it.
	var carried []db.ReleaseProblem
	if role == "admin" {
		carried, _ = h.db.CarriedForwardProblems(ctx, rel.ID)
	}
	return map[string]any{
		"Release":        rel,
		"RelVersion":     rel.Version,
		"Role":           role,
		"Problems":       problems,
		"Carried":        carried,
		"ProblemSummary": ps,
		"CanReport":      roleCanOperate(role),
		"CanAct":         role == "admin", // act on existing problems (fixed/verified/wontfix) — operators file only
	}
}

// writeReleaseProblemsResponse renders the htmx response for a problem change:
// out-of-band fragments that refresh the problem lists, counts and badges in place.
func (h *Handler) writeReleaseProblemsResponse(w http.ResponseWriter, r *http.Request, releaseID int) {
	rel, err := h.db.GetRelease(r.Context(), releaseID)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	_ = h.tmpl.ExecuteTemplate(w, "release-oob", h.releaseProblemsData(r, rel))
}

// ReleaseEditMeta updates a release's editable metadata (name + changelog) after
// creation, so the changelog stays maintainable through the release lifecycle.
func (h *Handler) ReleaseEditMeta(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	changelog := strings.TrimSpace(r.FormValue("changelog"))
	if err := h.db.SetReleaseMeta(r.Context(), id, name, changelog); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Optional release-date edit (HTML date input, yyyy-mm-dd). Setting created_at
	// re-dates the release for the list order and the OTA newer/older ranking.
	if d := strings.TrimSpace(r.FormValue("released_at")); d != "" {
		if t, perr := time.Parse("2006-01-02", d); perr == nil {
			_ = h.db.SetReleaseCreatedAt(r.Context(), id, t)
		}
	}
	h.audit(r, "release.edit_meta", strconv.Itoa(id), name)
	http.Redirect(w, r, fmt.Sprintf("/releases/%d", id), http.StatusSeeOther)
}

// PackageDelete removes a single package from a release.
func (h *Handler) PackageDelete(w http.ResponseWriter, r *http.Request) {
	relID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	pid, err := strconv.Atoi(r.PathValue("pid"))
	if err != nil {
		http.Error(w, "Invalid package ID", http.StatusBadRequest)
		return
	}
	if err := h.db.DeleteOTAPackage(r.Context(), pid); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/releases/%d", relID), http.StatusSeeOther)
}
