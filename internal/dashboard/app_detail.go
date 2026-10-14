package dashboard

import (
	"net/http"
	"sort"
	"strings"

	"mdm/internal/db"
)

// The app detail page: one app, every device that has it, and every version in service.
//
// This is the page a store is actually about, and ours can say something a consumer store
// cannot — not "4.5 stars from 2M people" but "108 of your 187 devices have this, 44 are on
// a version other than the library's, and 50 will not say". There are no screenshots to
// show because we store none; the version histogram is the honest thing to put in their
// place, since it is what you came to look at.
//
// Every number here comes from the coverage query already on the library page. Nothing new
// is collected to render this.

type appVersionRow struct {
	Version string // "" → the device reported no version
	Devices int
	Pct     int
	Latest  bool
}

type appDeviceRow struct {
	Serial  string
	Version string
	Venue   string
}

type appDetailView struct {
	Package string
	Name    string
	Icon    string
	Initial string
	Art     int

	// Library side
	InLibrary   bool
	LibVersion  string
	LibAdded    string
	LibVersions int
	ApkURL      string
	FamilyName  string
	Channels    int

	// Fleet side
	Fleet    int
	Devices  int
	OnLatest int
	Other    int
	Unknown  int
	Versions []appVersionRow
	Rows     []appDeviceRow
	More     int

	// What a send would reach, resolved before anything is sent.
	MissingCount int
	OtherCount   int
	CanSend      bool
}

// AppDetailPage renders /apps/{pkg}.
func (h *Handler) AppDetailPage(w http.ResponseWriter, r *http.Request) {
	pkg := strings.TrimSpace(r.PathValue("pkg"))
	if pkg == "" {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	apps, _ := h.db.ListApps(ctx)
	cov := h.appCoverage(ctx, apps)
	if cov == nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	c := cov.Of(pkg)
	fams, _, _ := h.libraryData(ctx)

	v := appDetailView{
		Package: pkg, Name: c.AppName, Art: artSlot(pkg),
		Fleet: cov.Totals.Devices, Devices: c.Devices,
		OnLatest: c.OnLatest, Unknown: c.Unknown,
		InLibrary: c.InLibrary, LibVersion: c.LibLatest,
	}
	v.Other = c.Known - c.OnLatest

	// Library side: name, icon, how many versions we hold, and the channel this belongs to.
	var newest *db.App
	for i := range apps {
		if apps[i].PackageName != pkg {
			continue
		}
		v.LibVersions++
		if newest == nil || apps[i].CreatedAt.After(newest.CreatedAt) {
			newest = &apps[i]
		}
	}
	if newest != nil {
		if newest.Name != "" {
			v.Name = newest.Name
		}
		v.Icon = newest.Icon
		v.ApkURL = newest.ApkURL
		v.LibAdded = newest.CreatedAt.Format("2 Jan 2006")
	}
	for i := range fams {
		for _, variant := range fams[i].Variants {
			if variant.Package == pkg {
				v.FamilyName = fams[i].Name
				v.Channels = len(fams[i].Variants)
			}
		}
	}
	if v.Name == "" {
		v.Name = pkg
	}
	if v.Icon == "" {
		v.Initial = string([]rune(v.Name)[0:1])
	}

	// Versions in service, biggest share first, with "no version" kept distinct — folding
	// it into a number would be the same lie as counting it as up to date.
	for _, vc := range c.Versions {
		row := appVersionRow{Version: vc.Version, Devices: vc.Devices, Latest: vc.Latest}
		if c.Devices > 0 {
			row.Pct = vc.Devices * 100 / c.Devices
		}
		v.Versions = append(v.Versions, row)
	}

	// Where it is installed. Capped: a 108-device app does not need 108 rows to make the
	// point, and the full list is a filter away on the Devices page.
	if rows, err := h.db.DevicesWithPackage(ctx, pkg); err == nil {
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Version > rows[j].Version })
		const cap = 40
		for i, d := range rows {
			if i >= cap {
				v.More = len(rows) - cap
				break
			}
			v.Rows = append(v.Rows, appDeviceRow{Serial: d.Serial, Version: d.Version, Venue: d.Venue})
		}
	}

	// What a send would reach. Resolved now so both buttons carry a count and nothing is
	// sent blind; the unknown-version devices are excluded by the same rule as the dialog.
	// Installs come from Releases (a role's app) or a device's lab view (anything else)
	// since the role model (14 Oct); the library page no longer sends installs itself.
	// The install route stays for the API and a change of plan.
	v.CanSend = false
	if v.CanSend {
		if t, err := h.db.SerialsMissingPackage(ctx, pkg); err == nil {
			v.MissingCount = len(t.Serials)
		}
		if t, err := h.db.SerialsOnOtherVersion(ctx, pkg, v.LibVersion); err == nil {
			v.OtherCount = len(t.Serials)
		}
	}

	h.render(w, r, "app_detail.html", map[string]any{
		"Title":      v.Name,
		"ActivePage": "apps",
		"App":        v,
	})
}
