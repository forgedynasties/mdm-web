package dashboard

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"mdm/internal/db"

	"github.com/google/uuid"
)

// Library grouping. A LibFamily is one tile: its variants (one per package) and
// each variant's versions, newest first. Versions of one package always group;
// variants group by the app_family_mode setting plus manual assignments.

type LibVersion struct {
	App    db.App
	Latest bool
}

type LibVariant struct {
	Label    string // prod | internal | uatv2 | ...
	Package  string
	Versions []LibVersion // newest first
	Latest   db.App
}

type LibFamily struct {
	ID          string // family uuid, or "pkg:<package>" for an ungrouped package
	FamilyID    *uuid.UUID
	Name        string
	BasePackage string
	Icon        string
	Variants    []LibVariant // prod first, then alphabetical
	Versions    int
	Grouped     bool // a real family (more than one variant, or persisted)
}

// Prod returns the variant to preselect: prod if present, else the first.
func (f LibFamily) Prod() LibVariant {
	for _, v := range f.Variants {
		if v.Label == "prod" {
			return v
		}
	}
	if len(f.Variants) > 0 {
		return f.Variants[0]
	}
	return LibVariant{}
}

// LibSuggestion is a proposed merge (suggest mode): packages that look like
// variants of one product but are not in a family yet.
type LibSuggestion struct {
	Name        string
	BasePackage string
	Packages    []string
	Variants    map[string]string // package -> variant label
}

// deriveRoots maps every package to the root package it looks like a variant of.
// A package is a variant of the longest other package it extends with one or
// more dot segments (aio.app.nugget.internal -> aio.app.nugget). Two packages
// with no such base that differ only in their last segment share their common
// prefix as a virtual root (aio.app.mpos.uatv2 + aio.app.mpos.internal ->
// aio.app.mpos), if that prefix has at least three segments.
func deriveRoots(pkgs []string) map[string]string {
	set := map[string]bool{}
	for _, p := range pkgs {
		set[p] = true
	}
	root := map[string]string{}
	for _, p := range pkgs {
		best := ""
		for q := range set {
			if q != p && strings.HasPrefix(p, q+".") && len(q) > len(best) {
				best = q
			}
		}
		if best != "" {
			root[p] = best
		}
	}
	// virtual roots among the still-rootless
	for _, p := range pkgs {
		if root[p] != "" {
			continue
		}
		i := strings.LastIndexByte(p, '.')
		if i < 0 {
			continue
		}
		prefix := p[:i]
		if strings.Count(prefix, ".") < 2 {
			continue
		}
		for _, q := range pkgs {
			if q == p || root[q] != "" {
				continue
			}
			if j := strings.LastIndexByte(q, '.'); j > 0 && q[:j] == prefix {
				root[p] = prefix
				root[q] = prefix
			}
		}
	}
	// collapse chains (a.b.c.d -> a.b.c -> a.b)
	for p, r := range root {
		for root[r] != "" && root[r] != r {
			r = root[r]
		}
		root[p] = r
	}
	return root
}

func variantLabel(pkg, root string) string {
	if pkg == root {
		return "prod"
	}
	if strings.HasPrefix(pkg, root+".") {
		return strings.TrimPrefix(pkg, root+".")
	}
	if i := strings.LastIndexByte(pkg, '.'); i >= 0 {
		return pkg[i+1:]
	}
	return pkg
}

var familyNameStrip = regexp.MustCompile(`(?i)[\s\-_(]*(internal|uat\w*|stage\w*|staging|beta|alpha|dev|debug|test\w*|qa|prod\w*)\)?\s*$`)

// familyName picks a display name: the base app's name, else the first name with
// a trailing variant word removed ("Nugget-INTERNAL" -> "Nugget").
func familyName(apps []db.App, root string) string {
	for _, a := range apps {
		if a.PackageName == root && a.Name != "" {
			return a.Name
		}
	}
	for _, a := range apps {
		if n := strings.TrimSpace(familyNameStrip.ReplaceAllString(a.Name, "")); n != "" {
			return n
		}
	}
	if i := strings.LastIndexByte(root, '.'); i >= 0 {
		return strings.ToUpper(root[i+1:][:1]) + root[i+2:]
	}
	return root
}

// versionLess orders version strings newest-first friendly: numeric segments
// compare as numbers, the rest as text.
func versionNewer(a, b string) bool {
	as, bs := strings.FieldsFunc(a, func(r rune) bool { return r == '.' || r == '-' }), strings.FieldsFunc(b, func(r rune) bool { return r == '.' || r == '-' })
	for i := 0; i < len(as) && i < len(bs); i++ {
		if as[i] == bs[i] {
			continue
		}
		an, bn := atoiPrefix(as[i]), atoiPrefix(bs[i])
		if an != bn {
			return an > bn
		}
		return as[i] > bs[i]
	}
	return len(as) > len(bs)
}

func atoiPrefix(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// buildLibrary groups apps into tiles. mode is the app_family_mode setting.
func buildLibrary(apps []db.App, families []db.AppFamily, mode string) ([]LibFamily, []LibSuggestion) {
	famByID := map[uuid.UUID]db.AppFamily{}
	for _, f := range families {
		famByID[f.ID] = f
	}
	// group rows by package, newest first
	byPkg := map[string][]db.App{}
	var pkgs []string
	for _, a := range apps {
		if _, ok := byPkg[a.PackageName]; !ok {
			pkgs = append(pkgs, a.PackageName)
		}
		byPkg[a.PackageName] = append(byPkg[a.PackageName], a)
	}
	sort.Strings(pkgs)
	for p := range byPkg {
		rows := byPkg[p]
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].VersionName != rows[j].VersionName {
				return versionNewer(rows[i].VersionName, rows[j].VersionName)
			}
			return rows[i].CreatedAt.After(rows[j].CreatedAt)
		})
	}
	// persisted families first
	tiles := map[string]*LibFamily{}
	var order []string
	assigned := map[string]bool{}
	for _, p := range pkgs {
		rows := byPkg[p]
		fid := rows[0].FamilyID
		if fid == nil {
			continue
		}
		f, ok := famByID[*fid]
		if !ok {
			continue
		}
		key := f.ID.String()
		t := tiles[key]
		if t == nil {
			id := f.ID
			t = &LibFamily{ID: key, FamilyID: &id, Name: f.Name, BasePackage: f.BasePackage, Grouped: true}
			tiles[key] = t
			order = append(order, key)
		}
		label := rows[0].Variant
		if label == "" {
			label = variantLabel(p, f.BasePackage)
		}
		t.Variants = append(t.Variants, mkVariant(label, p, rows))
		assigned[p] = true
	}
	// the rest: auto-derived (auto mode) or one tile per package (suggest/off)
	var rest []string
	for _, p := range pkgs {
		if !assigned[p] {
			rest = append(rest, p)
		}
	}
	roots := deriveRoots(rest)
	var suggestions []LibSuggestion
	if mode == "auto" {
		for _, p := range rest {
			r := roots[p]
			if r == "" || byPkg[p][0].FamilyPinned {
				r = p
			}
			key := "pkg:" + r
			t := tiles[key]
			if t == nil {
				var members []db.App
				for _, q := range rest {
					if q == r || (roots[q] == r && !byPkg[q][0].FamilyPinned) {
						members = append(members, byPkg[q]...)
					}
				}
				t = &LibFamily{ID: key, Name: familyName(members, r), BasePackage: r}
				tiles[key] = t
				order = append(order, key)
			}
			t.Variants = append(t.Variants, mkVariant(variantLabel(p, r), p, byPkg[p]))
		}
	} else {
		for _, p := range rest {
			key := "pkg:" + p
			t := &LibFamily{ID: key, Name: byPkg[p][0].Name, BasePackage: p}
			t.Variants = append(t.Variants, mkVariant("prod", p, byPkg[p]))
			tiles[key] = t
			order = append(order, key)
		}
		if mode == "suggest" {
			groups := map[string][]string{}
			for _, p := range rest {
				if r := roots[p]; r != "" && !byPkg[p][0].FamilyPinned {
					groups[r] = append(groups[r], p)
				}
			}
			for r, ps := range groups {
				if _, hasBase := byPkg[r]; hasBase && !byPkg[r][0].FamilyPinned {
					ps = append(ps, r)
				}
				if len(ps) < 2 {
					continue
				}
				sort.Strings(ps)
				var members []db.App
				vs := map[string]string{}
				for _, p := range ps {
					members = append(members, byPkg[p]...)
					vs[p] = variantLabel(p, r)
				}
				suggestions = append(suggestions, LibSuggestion{Name: familyName(members, r), BasePackage: r, Packages: ps, Variants: vs})
			}
			sort.Slice(suggestions, func(i, j int) bool { return suggestions[i].Name < suggestions[j].Name })
		}
	}
	var out []LibFamily
	for _, key := range order {
		t := tiles[key]
		sort.SliceStable(t.Variants, func(i, j int) bool {
			if (t.Variants[i].Label == "prod") != (t.Variants[j].Label == "prod") {
				return t.Variants[i].Label == "prod"
			}
			return t.Variants[i].Label < t.Variants[j].Label
		})
		for _, v := range t.Variants {
			t.Versions += len(v.Versions)
			if t.Icon == "" && v.Latest.Icon != "" {
				t.Icon = v.Latest.Icon
			}
		}
		if len(t.Variants) > 1 {
			t.Grouped = true
		}
		out = append(out, *t)
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, suggestions
}

func mkVariant(label, pkg string, rows []db.App) LibVariant {
	v := LibVariant{Label: label, Package: pkg, Latest: rows[0]}
	for i, a := range rows {
		v.Versions = append(v.Versions, LibVersion{App: a, Latest: i == 0})
	}
	return v
}

// applyAutoFamilies persists auto-derived groups so manual edits have rows to
// hang off and the other pages see the same grouping. Idempotent; skips pinned
// packages and packages already in a family.
func (h *Handler) applyAutoFamilies(ctx context.Context) {
	if h.cfg.AppFamilyMode() != "auto" {
		return
	}
	apps, err := h.db.ListApps(ctx)
	if err != nil {
		return
	}
	families, _ := h.db.ListAppFamilies(ctx)
	byBase := map[string]uuid.UUID{}
	for _, f := range families {
		if f.BasePackage != "" {
			byBase[f.BasePackage] = f.ID
		}
	}
	byPkg := map[string]db.App{}
	var free []string
	for _, a := range apps {
		if _, seen := byPkg[a.PackageName]; seen {
			continue
		}
		byPkg[a.PackageName] = a
		if a.FamilyID == nil && !a.FamilyPinned {
			free = append(free, a.PackageName)
		}
	}
	// derive over every package so a free variant can join an existing family's base
	var all []string
	for p := range byPkg {
		all = append(all, p)
	}
	sort.Strings(all)
	roots := deriveRoots(all)
	for _, p := range free {
		r := roots[p]
		if r == "" {
			// a base whose variants exist gets a family too
			hasKids := false
			for q, rq := range roots {
				if rq == p && q != p {
					hasKids = true
					break
				}
			}
			if !hasKids {
				continue
			}
			r = p
		}
		fid, ok := byBase[r]
		if !ok {
			var members []db.App
			for _, a := range apps {
				if a.PackageName == r || roots[a.PackageName] == r {
					members = append(members, a)
				}
			}
			id, err := h.db.CreateAppFamily(ctx, familyName(members, r), r)
			if err != nil {
				continue
			}
			fid = id
			byBase[r] = id
		}
		_ = h.db.SetAppFamily(ctx, p, &fid, variantLabel(p, r), false)
	}
	_ = h.db.DeleteEmptyAppFamilies(ctx)
}
