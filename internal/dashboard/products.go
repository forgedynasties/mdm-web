package dashboard

import (
	"mdm/internal/db"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"

	"mdm/internal/product"
)

// productNames caches the admin-set product display names (product_names table),
// keyed by normalized product key. Package-level for the same reason as
// productLabels: template funcs are built before any Handler exists.
var productNames atomic.Value // map[string]string

// productName is what a device of this product is called on its fleet row and device
// page: the admin-set name for a stock product, else the catalog display name of our
// own hardware ("TSAI" for the T7), else "".
func productName(key string) string {
	if n := adminProductName(key); n != "" {
		return n
	}
	// Legacy rows with no product key are the default product (T7) too.
	if p, ok := product.Resolve(key); ok || product.Normalize(key) == "" {
		return p.DisplayName
	}
	return ""
}

// adminProductName is only the admin-set name (Products page), or "".
func adminProductName(key string) string {
	if m, ok := productNames.Load().(map[string]string); ok {
		return m[product.Normalize(key)]
	}
	return ""
}

func (h *Handler) reloadProductNames(ctx context.Context) {
	m, err := h.db.ProductNames(ctx)
	if err != nil {
		log.Printf("product names: %v", err)
		return
	}
	productNames.Store(m)
}

// ProductsPage is the Hardware page (static/hardware-class-models-demos.html, H3): the
// classes in a side list, one class at a time on the right with its app and the models
// attached to it. A device's class is its model's class, so this is where a class is
// decided. Our own hardware is built in to its class; any other model is attached here,
// before or after its first device enrolls. Super admins only.
func (h *Handler) ProductsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	h.reloadProductNames(ctx)
	productions, _ := h.db.ListProductions(ctx, h.connectedSlice())
	rows, err := h.db.ProductOverview(ctx)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Models with no device yet: our catalog boards, and models attached ahead of time.
	seen := map[string]bool{}
	for _, p := range rows {
		seen[p.Key] = true
	}
	for _, cp := range product.All() {
		if !seen[cp.Key] {
			rows = append(rows, db.ProductRow{Key: cp.Key})
			seen[cp.Key] = true
		}
	}
	if attached, err := h.db.AttachedModels(ctx); err == nil {
		for _, p := range attached {
			if !seen[p.Key] {
				rows = append(rows, p)
				seen[p.Key] = true
			}
		}
	}
	modes, _ := h.db.FirmwareModes(ctx)
	roleApps, _ := h.db.ListRoleApps(ctx)
	appOf := map[string]string{}
	for _, ra := range roleApps {
		appOf[ra.Role] = ra.BasePackage
	}
	library, _ := h.db.ListApps(ctx)

	type model struct {
		Key, Display, Reported, Name, Role string
		Known, CanFw                       bool
		Devices, Firmware, DPC, Lite       int
		FwMode                             string // prod | lab | off
	}
	type classGroup struct {
		Key, Label, App string
		Devices         int
		Models          []model
		On              bool
	}
	var groups []classGroup
	var none []model
	idx := map[string]int{}
	for _, c := range product.Classes() {
		idx[c] = len(groups)
		groups = append(groups, classGroup{Key: c, Label: product.ClassLabel(c), App: appOf[c]})
	}
	var stock []model // every attachable model, for the "+ Add model" list
	for _, p := range rows {
		m := model{
			Key: p.Key, Known: product.IsKnown(p.Key), Reported: modelLabel(p.Manufacturer, p.Model),
			Devices: p.Devices, Firmware: p.Firmware, DPC: p.DPC, Lite: p.Lite, Name: p.Name, Role: p.Role,
			FwMode: db.FirmwareModeOf(modes, p.Key),
		}
		if m.Known {
			cp, _ := product.Resolve(p.Key)
			m.Role, m.CanFw = cp.Class, true
		}
		m.Display = productLabel(p.Key)
		if !m.Known {
			stock = append(stock, m)
		}
		i, ok := idx[m.Role]
		if !ok {
			none = append(none, m)
			continue
		}
		groups[i].Models = append(groups[i].Models, m)
		groups[i].Devices += m.Devices
	}
	// Built-in boards first in catalog order, then attached models by device count.
	order := map[string]int{}
	for i, cp := range product.All() {
		order[cp.Key] = i
	}
	for i := range groups {
		ms := groups[i].Models
		sort.SliceStable(ms, func(a, b int) bool {
			if ms[a].Known != ms[b].Known {
				return ms[a].Known
			}
			if ms[a].Known {
				return order[ms[a].Key] < order[ms[b].Key]
			}
			return ms[a].Devices > ms[b].Devices
		})
	}
	// "Other" only shows once something is in it.
	kept := groups[:0]
	for _, g := range groups {
		if g.Key != product.ClassOther || len(g.Models) > 0 || g.App != "" {
			kept = append(kept, g)
		}
	}
	sel := r.URL.Query().Get("class")
	if sel == "none" && len(none) == 0 {
		sel = ""
	}
	var cur *classGroup
	for i := range kept {
		if kept[i].Key == sel {
			cur = &kept[i]
		}
	}
	if cur == nil && sel != "none" {
		// Default: the first class with devices, else the first class.
		for i := range kept {
			if kept[i].Devices > 0 {
				cur = &kept[i]
				break
			}
		}
		if cur == nil && len(kept) > 0 {
			cur = &kept[0]
		}
	}
	if cur != nil {
		cur.On = true
		sel = cur.Key
	}
	noneDevices := 0
	for _, m := range none {
		noneDevices += m.Devices
	}
	var candidates []model
	for _, m := range stock {
		if m.Role != sel {
			candidates = append(candidates, m)
		}
	}
	h.render(w, r, "products.html", map[string]any{
		"Title":       "Hardware",
		"ActivePage":  "products",
		"Classes":     kept,
		"Cur":         cur,
		"Sel":         sel,
		"None":        none,
		"NoneDevices": noneDevices,
		"Candidates":  candidates,
		"ClassKeys":   product.Classes(),
		"Choices":     appChoicesFrom(library),
		"Productions": productions,
		"Flash":       r.URL.Query().Get("flash"),
	})
}

// ProductAttach attaches a stock model to a class (or, with no class, detaches it), from
// the Hardware page: a model picked from the fleet's list or a model key typed in for
// hardware that has not enrolled yet. Its devices take the class now and at every later
// enrollment.
func (h *Handler) ProductAttach(w http.ResponseWriter, r *http.Request) {
	back := strings.TrimSpace(r.FormValue("back"))
	dest := "/products"
	if back != "" {
		dest += "?class=" + url.QueryEscape(back)
	}
	key := product.Normalize(r.FormValue("model_key"))
	if key == "" {
		key = product.Normalize(r.FormValue("model"))
	}
	if key == "" {
		h.hxDoneToast(w, r, dest, "Choose a model or type its key", "error")
		return
	}
	if product.IsKnown(key) {
		h.hxDoneToast(w, r, dest, "Our own hardware is built in to its class", "error")
		return
	}
	role := strings.TrimSpace(r.FormValue("role"))
	if role != "" && !product.IsClass(role) {
		h.hxDoneToast(w, r, dest, "Unknown class", "error")
		return
	}
	n, err := h.db.SetProductRole(r.Context(), key, role, h.currentUsername(r))
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "product.role", key, role)
	plural := map[bool]string{true: "", false: "s"}[n == 1]
	msg := fmt.Sprintf("%s detached · %d device%s lost its class", productLabel(key), n, plural)
	if role != "" {
		msg = fmt.Sprintf("%s attached to %s · %d device%s updated", productLabel(key), product.ClassLabel(role), n, plural)
	}
	h.hxDoneToast(w, r, dest, msg, "success")
}

// ProductRename sets or clears a stock product's display name. Catalog products
// (our own hardware) keep their catalog label.
func (h *Handler) ProductRename(w http.ResponseWriter, r *http.Request) {
	key := product.Normalize(r.PathValue("key"))
	if key == "" || product.IsKnown(key) {
		h.hxDoneToast(w, r, "/products", "Only stock products (Standard MDM / MDM Lite hardware) can be renamed", "error")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if r.FormValue("reset") == "1" {
		name = ""
	}
	if len(name) > 60 {
		h.hxDoneToast(w, r, "/products", "Keep the name under 60 characters", "error")
		return
	}
	if err := h.db.SetProductName(r.Context(), key, name, h.currentUsername(r)); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.reloadProductNames(r.Context())
	h.audit(r, "product.rename", key, name)
	msg := "Renamed to " + name
	if name == "" {
		msg = "Name reset — showing what the devices report"
	}
	h.hxDoneToast(w, r, "/products", msg, "success")
}

// ProductSetRole sets a stock model's default role and applies it to the model's devices.
func (h *Handler) ProductSetRole(w http.ResponseWriter, r *http.Request) {
	key := product.Normalize(r.PathValue("key"))
	if key == "" || product.IsKnown(key) {
		h.hxDoneToast(w, r, "/products", "Our own hardware takes its product from the catalog", "error")
		return
	}
	role := strings.TrimSpace(r.FormValue("role"))
	if role != "" && !product.IsClass(role) {
		h.hxDoneToast(w, r, "/products", "Unknown product", "error")
		return
	}
	n, err := h.db.SetProductRole(r.Context(), key, role, h.currentUsername(r))
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "product.role", key, role)
	msg := "Product cleared — new devices of this model will need one"
	if role != "" {
		msg = fmt.Sprintf("Set to %s · %d device%s updated", product.ClassLabel(role), n, map[bool]string{true: "", false: "s"}[n == 1])
	}
	h.hxDoneToast(w, r, "/products", msg, "success")
}
