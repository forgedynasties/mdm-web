package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// libraryData is what every page with an app picker needs: tiles per family,
// merge suggestions (suggest mode) and the mode itself.
func (h *Handler) libraryData(ctx context.Context) (families []LibFamily, suggestions []LibSuggestion, mode string) {
	mode = h.cfg.AppFamilyMode()
	h.applyAutoFamilies(ctx)
	apps, _ := h.db.ListApps(ctx)
	fams, _ := h.db.ListAppFamilies(ctx)
	families, suggestions = buildLibrary(apps, fams, mode)
	return
}

func (h *Handler) libraryRedirect(w http.ResponseWriter, r *http.Request, msg, typ string) {
	next := r.FormValue("next")
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/apps"
	}
	// The library forms are hx-boosted: a redirect makes htmx swap the page, a 204
	// would leave it as it was.
	_ = msg
	_ = typ
	h.hxRedirect(w, r, next)
}

// AppFamilyCreate merges packages into a new family (from a suggestion or by hand).
func (h *Handler) AppFamilyCreate(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	base := strings.TrimSpace(r.FormValue("base_package"))
	pkgs := parseSerialsField(r.Form["packages"])
	if name == "" || len(pkgs) == 0 {
		h.libraryRedirect(w, r, "Name the family and pick at least one package", "error")
		return
	}
	if base == "" {
		base = deriveRoots(pkgs)[pkgs[0]]
		if base == "" {
			base = pkgs[0]
		}
	}
	id, err := h.db.CreateAppFamily(r.Context(), name, base)
	if err != nil {
		h.libraryRedirect(w, r, "Could not create the family", "error")
		return
	}
	for _, p := range pkgs {
		_ = h.db.SetAppFamily(r.Context(), p, &id, variantLabel(p, base), true)
	}
	_ = h.db.DeleteEmptyAppFamilies(r.Context())
	h.audit(r, "apps.family_create", name, strings.Join(pkgs, ","))
	h.libraryRedirect(w, r, fmt.Sprintf("Merged %d package%s into %s", len(pkgs), plural(len(pkgs)), name), "success")
}

func (h *Handler) AppFamilyRename(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	name := strings.TrimSpace(r.FormValue("name"))
	if err != nil || name == "" {
		h.libraryRedirect(w, r, "Give the family a name", "error")
		return
	}
	if err := h.db.RenameAppFamily(r.Context(), id, name); err != nil {
		h.libraryRedirect(w, r, "Could not rename", "error")
		return
	}
	h.audit(r, "apps.family_rename", id.String(), name)
	h.libraryRedirect(w, r, "Renamed to "+name, "success")
}

// AppFamilyUngroup splits a family back into separate packages (pinned, so auto
// mode leaves them alone).
func (h *Handler) AppFamilyUngroup(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := h.db.DeleteAppFamily(r.Context(), id); err != nil {
		h.libraryRedirect(w, r, "Could not ungroup", "error")
		return
	}
	h.audit(r, "apps.family_ungroup", id.String(), "")
	h.libraryRedirect(w, r, "Ungrouped · each package is its own tile again", "success")
}

// AppPackageAssign moves one package into a family (family_id), or detaches it
// (empty family_id). Manual either way, so pinned.
func (h *Handler) AppPackageAssign(w http.ResponseWriter, r *http.Request) {
	pkg := strings.TrimSpace(r.FormValue("package"))
	if pkg == "" {
		h.libraryRedirect(w, r, "No package given", "error")
		return
	}
	variant := strings.TrimSpace(strings.ToLower(r.FormValue("variant")))
	fidRaw := strings.TrimSpace(r.FormValue("family_id"))
	if fidRaw == "" || r.FormValue("detach") == "1" {
		if err := h.db.SetAppFamily(r.Context(), pkg, nil, "", true); err != nil {
			h.libraryRedirect(w, r, "Could not detach", "error")
			return
		}
		_ = h.db.DeleteEmptyAppFamilies(r.Context())
		h.audit(r, "apps.package_detach", pkg, "")
		h.libraryRedirect(w, r, pkg+" is its own tile now", "success")
		return
	}
	fid, err := uuid.Parse(fidRaw)
	if err != nil {
		h.libraryRedirect(w, r, "Bad family", "error")
		return
	}
	if variant == "" {
		base := ""
		for _, f := range mustList(h.db.ListAppFamilies(r.Context())) {
			if f.ID == fid {
				base = f.BasePackage
			}
		}
		variant = variantLabel(pkg, base)
	}
	if err := h.db.SetAppFamily(r.Context(), pkg, &fid, variant, true); err != nil {
		h.libraryRedirect(w, r, "Could not move the package", "error")
		return
	}
	_ = h.db.DeleteEmptyAppFamilies(r.Context())
	h.audit(r, "apps.package_assign", pkg, fid.String()+" as "+variant)
	h.libraryRedirect(w, r, pkg+" added as "+variant, "success")
}

func mustList[T any](v []T, _ error) []T { return v }

// SettingsSetAppFamilyMode switches auto / suggest / off.
func (h *Handler) SettingsSetAppFamilyMode(w http.ResponseWriter, r *http.Request) {
	if err := h.cfg.SetAppFamilyMode(r.FormValue("app_family_mode")); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "settings.app_family_mode", "", h.cfg.AppFamilyMode())
	h.libraryRedirect(w, r, "Variant grouping: "+h.cfg.AppFamilyMode(), "success")
}
