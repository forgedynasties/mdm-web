package dashboard

import (
	"context"
	"fmt"
	"hash/fnv"
	"sort"

	"mdm/internal/db"
)

// The store view of /apps: search, chips, and shelves of icon cards.
//
// The page used to stack six blocks of chrome — a header whose copy pointed at a page that
// no longer exists, a wall of finding banners, a card header, a merge-suggestion strip and
// an upload drawer — before the first app appeared. A store is the one kind of page where
// the goods have to come first.
//
// Each shelf answers a different question, and one of them ("in the library, on no device")
// is a thing nothing in the product could show before: uploads that were never rolled out.

// storeCard is one app on a shelf.
type storeCard struct {
	Name     string
	Package  string
	Icon     string // library icon if we have one
	Initial  string // first letter, when we do not
	FamilyID string // opens the existing library modal
	Art      int    // 0..5, a stable palette slot derived from the package name
	Devices  int
	Fleet    int
	Meta     string // the fleet fact under the name
	CTA      string // button label
	CTAKind  string // open | install | reconcile | inventory
	// Search haystack: name, package and every version seen, lowercased by the template's
	// consumer. Searching by version is how people actually look ("who has 46.0.0?").
	Search string
}

type storeShelf struct {
	ID    string
	Title string
	Note  string
	Cards []storeCard
}

type storeView struct {
	// Chips are the shelves plus "All apps", which has to exist: shelves scroll sideways
	// and therefore hide things, and an inventory you audit needs a view that shows
	// everything. That view is the grid the page already had.
	Shelves []storeShelf
	Fleet   int
}

// artSlot picks a stable gradient per package. We have no store artwork — no screenshots,
// banners or descriptions — so the art is generated from the package name and stays put
// between renders, which reads as deliberate rather than random.
func artSlot(pkg string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(pkg))
	return int(h.Sum32() % 6)
}

func cardFor(pkg string, cov pkgCoverage, fam *LibFamily, variant *LibVariant, fleet int) storeCard {
	c := storeCard{Package: pkg, Fleet: fleet, Devices: cov.Devices, Art: artSlot(pkg)}
	c.Name = cov.AppName
	if variant != nil && variant.Latest.Name != "" {
		c.Name = variant.Latest.Name
		c.Icon = variant.Latest.Icon
	}
	if fam != nil {
		c.FamilyID = fam.ID
		if c.Icon == "" {
			c.Icon = fam.Icon
		}
	}
	if c.Name == "" {
		c.Name = pkg
	}
	if c.Icon == "" {
		c.Initial = string([]rune(c.Name)[0:1])
	}
	c.Search = c.Name + " " + pkg
	for _, v := range cov.Versions {
		if v.Version != "" {
			c.Search += " " + v.Version
		}
	}
	return c
}

// buildStore assembles the shelves. Order is deliberate: what is wrong, what the fleet
// runs, what was uploaded and never used, what arrived recently.
func buildStore(fams []LibFamily, cov *coverageView, apps []db.App) *storeView {
	if cov == nil {
		return nil
	}
	fleet := cov.Totals.Devices
	// Index the library by package so a card can borrow its icon and open its modal.
	famByPkg := map[string]*LibFamily{}
	varByPkg := map[string]*LibVariant{}
	for i := range fams {
		f := &fams[i]
		for j := range f.Variants {
			v := &f.Variants[j]
			famByPkg[v.Package] = f
			varByPkg[v.Package] = v
		}
	}

	sv := &storeView{Fleet: fleet}

	// ── On your fleet: what is actually out there, most-installed first ──
	var onFleet []storeCard
	for pkg, c := range cov.ByPkg {
		if c.Devices == 0 {
			continue
		}
		card := cardFor(pkg, c, famByPkg[pkg], varByPkg[pkg], fleet)
		card.Meta = fmt.Sprintf("on %d of %d", c.Devices, fleet)
		if c.Unknown > 0 {
			card.Meta += fmt.Sprintf(" · %d unknown", c.Unknown)
		}
		card.CTA, card.CTAKind = "Open", "open"
		onFleet = append(onFleet, card)
	}
	sort.SliceStable(onFleet, func(i, j int) bool { return onFleet[i].Devices > onFleet[j].Devices })
	if len(onFleet) > 0 {
		sv.Shelves = append(sv.Shelves, storeShelf{
			ID: "fleet", Title: "On your fleet", Note: "by how many devices report it", Cards: onFleet,
		})
	}

	// ── Recently added: the library's newest uploads ──
	recent := append([]db.App{}, apps...)
	sort.SliceStable(recent, func(i, j int) bool { return recent[i].CreatedAt.After(recent[j].CreatedAt) })
	var added []storeCard
	rseen := map[string]bool{}
	for _, a := range recent {
		if rseen[a.PackageName] || len(added) >= 10 {
			continue
		}
		rseen[a.PackageName] = true
		card := cardFor(a.PackageName, cov.Of(a.PackageName), famByPkg[a.PackageName], varByPkg[a.PackageName], fleet)
		card.Meta = "added " + a.CreatedAt.Format("2 Jan")
		if a.VersionName != "" {
			card.Meta = a.VersionName + " · " + card.Meta
		}
		card.CTA, card.CTAKind = "Open", "open"
		added = append(added, card)
	}
	if len(added) > 0 {
		sv.Shelves = append(sv.Shelves, storeShelf{
			ID: "recent", Title: "Recently added to the library", Cards: added,
		})
	}
	return sv
}

func (h *Handler) appStore(ctx context.Context, fams []LibFamily, cov *coverageView, apps []db.App) *storeView {
	return buildStore(fams, cov, apps)
}
