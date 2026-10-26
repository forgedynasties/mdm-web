package dashboard

import (
	"net/http"
	"sort"
	"strings"

	prod "mdm/internal/product"
)

// The app page: one package, every device that has it, every version in service, and
// (D1, 21 Oct 2026) what to do about it, with the library's versions, the family's
// channels and the family tools in a rail. The builder is app_d1.go.
//
// This is the page a store is actually about, and ours can say something a consumer store
// cannot — not "4.5 stars from 2M people" but "108 of your 187 devices have this, 44 are on
// a version other than the library's, and 50 will not say". Every fleet number comes from
// the coverage query the library page already runs, plus the device rows behind it.

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
	fams, _, mode := h.libraryData(ctx)

	in := d1Inputs{Package: pkg, Cov: cov.Of(pkg), Fleet: cov.Totals.Devices, Apps: apps, Families: fams,
		Siblings: map[string]map[string]bool{}, SiblingCov: map[string]pkgCoverage{}}
	acc := h.access(r)
	if rows, err := h.db.DevicesWithPackage(ctx, pkg); err == nil {
		for _, d := range rows {
			if acc.visible(d.DeviceID) {
				in.Devices = append(in.Devices, d)
			}
		}
	}
	// The other channels of its family: who runs them, so "without it" can say how many
	// of those devices already have the app under another package.
	for _, f := range fams {
		mine := false
		for _, vr := range f.Variants {
			if vr.Package == pkg {
				mine = true
			}
		}
		if !mine {
			continue
		}
		for _, vr := range f.Variants {
			if vr.Package == pkg {
				continue
			}
			in.SiblingCov[vr.Package] = cov.Of(vr.Package)
			set := map[string]bool{}
			if rows, err := h.db.DevicesWithPackage(ctx, vr.Package); err == nil {
				for _, d := range rows {
					if acc.visible(d.DeviceID) {
						set[d.Serial] = true
					}
				}
			}
			in.Siblings[vr.Package] = set
		}
	}
	// What a send would reach, resolved now so both buttons carry a count and nothing is
	// sent blind; the unknown-version devices are excluded by the same rule as the chooser.
	in.CanSend = in.Cov.InLibrary && h.commandTypeAllowed(h.role(r), "install_apk")
	if in.CanSend {
		if t, err := h.db.SerialsMissingPackage(ctx, pkg); err == nil {
			in.Missing = t.Serials
		}
		if t, err := h.db.SerialsOnOtherVersion(ctx, pkg, in.Cov.LibLatest); err == nil {
			in.OtherCount = len(t.Serials)
		}
	}
	v := buildAppD1(in)

	// The products that run this app (P1, 23 Oct 2026): the page says so and links to the
	// product on Apps › By product, where its build and units are.
	type appProduct struct{ Role, Label, Variant string }
	var productOf []appProduct
	for role, base := range h.db.RoleAppBases(ctx) {
		switch {
		case base == "":
		case pkg == base:
			productOf = append(productOf, appProduct{role, prod.ClassLabel(role), ""})
		case strings.HasPrefix(pkg, base+"."):
			productOf = append(productOf, appProduct{role, prod.ClassLabel(role), strings.TrimPrefix(pkg, base+".")})
		}
	}
	sort.Slice(productOf, func(i, j int) bool { return productOf[i].Label < productOf[j].Label })
	h.render(w, r, "app_detail.html", map[string]any{
		"ProductOf":     productOf,
		"Title":         v.Name,
		"ActivePage":    "apps",
		"App":           v,
		"Families":      fams, // the family tools' "add to a family" list
		"AppFamilyMode": mode,
	})
}
