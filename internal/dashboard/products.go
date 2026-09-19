package dashboard

import (
	"context"
	"log"
	"net/http"
	"strings"
	"sync/atomic"

	"mdm/internal/product"
)

// productNames caches the admin-set product display names (product_names table),
// keyed by normalized product key. Package-level for the same reason as
// productLabels: template funcs are built before any Handler exists.
var productNames atomic.Value // map[string]string

// productName is the admin-set display name for a product key, or "".
func productName(key string) string {
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

// ProductsPage lists every product the fleet reports, with a rename for the stock
// ones (MDM DPC and MDM Lite hardware, whose product key is a chip or board name).
// Admin only for now.
func (h *Handler) ProductsPage(w http.ResponseWriter, r *http.Request) {
	h.reloadProductNames(r.Context())
	rows, err := h.db.ProductOverview(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	type view struct {
		Key, Display, Reported, Catalog string
		Known                           bool
		Devices, Firmware, DPC, Lite    int
		Name, UpdatedBy                 string
	}
	var ours, stock []view
	for _, p := range rows {
		v := view{
			Key: p.Key, Known: product.IsKnown(p.Key), Reported: modelLabel(p.Manufacturer, p.Model),
			Devices: p.Devices, Firmware: p.Firmware, DPC: p.DPC, Lite: p.Lite, Name: p.Name, UpdatedBy: p.UpdatedBy,
		}
		v.Catalog = product.Label(p.Key)
		v.Display = productLabel(p.Key)
		if v.Known {
			ours = append(ours, v)
		} else {
			stock = append(stock, v)
		}
	}
	h.render(w, r, "products.html", map[string]any{
		"Title": "Products",
		"Ours":  ours,
		"Stock": stock,
	})
}

// ProductRename sets or clears a stock product's display name. Catalog products
// (our own hardware) keep their catalog label.
func (h *Handler) ProductRename(w http.ResponseWriter, r *http.Request) {
	key := product.Normalize(r.PathValue("key"))
	if key == "" || product.IsKnown(key) {
		h.hxDoneToast(w, r, "/products", "Only stock products (MDM DPC / MDM Lite hardware) can be renamed", "error")
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
