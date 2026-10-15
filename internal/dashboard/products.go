package dashboard

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"

	"mdm/internal/product"

	"github.com/google/uuid"
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

// ProductsPage is the old Hardware page, now part of Lineup: it sends the visitor to the
// same role there.
func (h *Handler) ProductsPage(w http.ResponseWriter, r *http.Request) {
	dest := "/lineup"
	if c := r.URL.Query().Get("class"); c != "" {
		dest += "?role=" + url.QueryEscape(c)
	}
	http.Redirect(w, r, dest, http.StatusMovedPermanently)
}

// PipelineRedirect sends the old Releases address to Lineup, keeping the query.
func (h *Handler) PipelineRedirect(w http.ResponseWriter, r *http.Request) {
	dest := "/lineup"
	if q := r.URL.RawQuery; q != "" {
		dest += "?" + q
	}
	http.Redirect(w, r, dest, http.StatusMovedPermanently)
}

// ProductAttach attaches a stock model to a class (or, with no class, detaches it), from
// the Hardware page: a model picked from the fleet's list or a model key typed in for
// hardware that has not enrolled yet. Its devices take the class now and at every later
// enrollment.
func (h *Handler) ProductAttach(w http.ResponseWriter, r *http.Request) {
	back := strings.TrimSpace(r.FormValue("back"))
	dest := "/lineup"
	if back != "" {
		dest += "?role=" + url.QueryEscape(back)
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
		h.hxDoneToast(w, r, "/lineup", "Only stock products (Standard MDM / MDM Lite hardware) can be renamed", "error")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if r.FormValue("reset") == "1" {
		name = ""
	}
	if len(name) > 60 {
		h.hxDoneToast(w, r, "/lineup", "Keep the name under 60 characters", "error")
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
	h.hxDoneToast(w, r, "/lineup", msg, "success")
}

// ProductSetRole sets a stock model's default role and applies it to the model's devices.
func (h *Handler) ProductSetRole(w http.ResponseWriter, r *http.Request) {
	key := product.Normalize(r.PathValue("key"))
	if key == "" || product.IsKnown(key) {
		h.hxDoneToast(w, r, "/lineup", "Our own hardware takes its product from the catalog", "error")
		return
	}
	role := strings.TrimSpace(r.FormValue("role"))
	if role != "" && !product.IsClass(role) {
		h.hxDoneToast(w, r, "/lineup", "Unknown product", "error")
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
	h.hxDoneToast(w, r, "/lineup", msg, "success")
}

// modelOpt is one hardware model a production run can be linked to.
type modelOpt struct{ Key, Label string }

// modelOptions lists every model the dashboard knows: our catalog boards, every model the
// fleet has reported, and models attached to a class ahead of their first device.
func (h *Handler) modelOptions(ctx context.Context) []modelOpt {
	seen := map[string]bool{}
	var out []modelOpt
	add := func(k string) {
		k = product.Normalize(k)
		if k == "" || seen[k] {
			return
		}
		seen[k] = true
		out = append(out, modelOpt{k, productLabel(k)})
	}
	for _, cp := range product.All() {
		add(cp.Key)
	}
	if rows, err := h.db.ProductOverview(ctx); err == nil {
		for _, p := range rows {
			add(p.Key)
		}
	}
	if rows, err := h.db.AttachedModels(ctx); err == nil {
		for _, p := range rows {
			add(p.Key)
		}
	}
	return out
}

func (h *Handler) isModelOption(ctx context.Context, key string) bool {
	for _, m := range h.modelOptions(ctx) {
		if m.Key == key {
			return true
		}
	}
	return false
}

// ProductionSetModel links a production run to the hardware model it built, or unlinks it.
func (h *Handler) ProductionSetModel(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid production ID", http.StatusBadRequest)
		return
	}
	back := r.FormValue("back")
	if !strings.HasPrefix(back, "/") || strings.HasPrefix(back, "//") {
		back = "/productions/" + id.String()
	}
	model := product.Normalize(r.FormValue("model"))
	if model != "" && !h.isModelOption(r.Context(), model) {
		h.hxDoneToast(w, r, back, "Unknown model", "error")
		return
	}
	if err := h.db.SetProductionModel(r.Context(), id, model); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "production.model", id.String(), model)
	msg := "Production unlinked from its model"
	if model != "" {
		msg = "Production linked to " + productLabel(model)
	}
	h.hxDoneToast(w, r, back, msg, "success")
}
