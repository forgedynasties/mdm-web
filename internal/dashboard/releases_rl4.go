package dashboard

import (
	"fmt"
	"net/url"
	"strings"

	"mdm/internal/product"
)

// Releases RL4 (picked Nov 2026, "a table with a preview, Fleet's twin"): the release list is
// one table in Fleet's layout (rail of products and views, search, sort, rows), and the
// release picked in it is explained in a preview pane beside it. Filtering and sorting run in
// the page (static/js/coll_list.js); a row opens ?release=<id> so the pane is server-built
// (releases_r2.go) as before.

// rl4Row is one table row: the version row plus what the extra columns show.
type rl4Row struct {
	versionRow
	Href      string
	Flags     string // data-flags: rel | dev | rep | hid
	Search    string
	DateUnix  int64
	Roll      *rolloutView // the newest rollout of this release, if any
	Installed int
	Failed    int
	Moving    int
	Rest      int
	Label     string // under the bar: "141 of 188 installed" / "12 devices"
	Updating  bool
	CrashN    int
	ProdKey   string // the product's catalog key, for the rail's product filter
}

// rl4Counts is what the rail shows next to its views.
type rl4Counts struct {
	Rel, Dev, Rep, Hid, Updating int
	ByProduct                    map[string]int // releases and reported builds shown by default, per product key
}

func buildRL4(rows, hidden []versionRow, byRelease map[int][]rolloutView, filterProduct string) ([]rl4Row, rl4Counts) {
	// Share of a product's devices on a build, for releases with no rollout of their own.
	perProd := map[string]int{}
	for _, v := range rows {
		perProd[v.Product] += v.DeviceCount
	}
	var out []rl4Row
	c := rl4Counts{ByProduct: map[string]int{}}
	add := func(v versionRow, isHidden bool) {
		r := rl4Row{versionRow: v}
		q := ""
		if filterProduct != "" {
			q = "&product=" + url.QueryEscape(filterProduct)
		}
		if v.Tracked && v.ReleaseID != nil {
			r.Href = fmt.Sprintf("/releases?release=%d%s", *v.ReleaseID, q)
		} else {
			r.Href = "/releases?build=" + url.QueryEscape(v.Version) + q
		}
		switch {
		case isHidden:
			r.Flags = "hid"
			c.Hid++
		case !v.Tracked:
			r.Flags = "rep"
			c.Rep++
		default:
			r.Flags = "rel"
			c.Rel++
		}
		if v.Product != "" {
			p, _ := product.Resolve(v.Product)
			r.ProdKey = p.Key
			if !isHidden {
				c.ByProduct[r.ProdKey]++
			}
		}
		if v.ReleasedAt != nil {
			r.DateUnix = v.ReleasedAt.Unix()
		}
		if v.Crash != nil {
			r.CrashN = v.Crash.Issues
		}
		if v.ReleaseID != nil {
			if rs := byRelease[*v.ReleaseID]; len(rs) > 0 {
				x := rs[0]
				r.Roll = &x
				for _, y := range rs {
					if y.Status == "active" {
						r.Updating = true
					}
				}
				r.Installed, r.Failed, r.Moving = x.Installed, x.Failed+x.Trouble(), x.Moving()
				r.Rest = x.Total - r.Installed - r.Failed - r.Moving
				if r.Rest < 0 {
					r.Rest = 0
				}
				r.Label = fmt.Sprintf("%d of %d installed", x.Installed, x.Total)
			}
		}
		if r.Roll == nil {
			r.Installed = v.DeviceCount
			if t := perProd[v.Product]; t > v.DeviceCount {
				r.Rest = t - v.DeviceCount
			}
			if v.DeviceCount > 0 {
				r.Label = fmt.Sprintf("%d device%s", v.DeviceCount, rl4Plural(v.DeviceCount))
			} else {
				r.Label = "no devices"
			}
		}
		if r.Updating {
			c.Updating++
		}
		r.Search = strings.ToLower(v.Version + " " + v.Name + " " + v.Product + " " + v.Status)
		out = append(out, r)
	}
	for _, v := range rows {
		add(v, false)
	}
	for _, v := range hidden {
		add(v, true)
	}
	return out, c
}

func rl4Plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
