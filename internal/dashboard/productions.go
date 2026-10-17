package dashboard

import (
	"encoding/csv"
	"fmt"
	_ "image/gif"
	_ "image/jpeg"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mdm/internal/db"
	"mdm/internal/product"

	"github.com/google/uuid"
)

// ── Productions ───────────────────────────────────────────────────────────────

func (h *Handler) ProductionList(w http.ResponseWriter, r *http.Request) {
	productions, err := h.db.ListProductions(r.Context(), h.connectedSlice())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.render(w, r, "productions.html", map[string]any{
		"Productions": productions,
	})
}

func (h *Handler) ProductionNew(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "production_form.html", map[string]any{"Models": h.modelOptions(r.Context())})
}

func (h *Handler) ProductionCreate(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()

	name := strings.TrimSpace(r.FormValue("name"))
	productCode := strings.ToUpper(strings.TrimSpace(r.FormValue("product_code")))
	modelCode := strings.TrimSpace(r.FormValue("model_code"))
	variant := r.FormValue("variant")
	if variant == "" {
		variant = "0"
	}
	sku := strings.ToUpper(strings.TrimSpace(r.FormValue("sku")))
	if sku == "" {
		sku = "AA"
	}
	// Model code is the zero-padded integer part of the model size; accept a single
	// digit ("6") and pad it to two ("06") to match the field's documented contract.
	if len(modelCode) == 1 {
		modelCode = "0" + modelCode
	}
	batchMonth, _ := strconv.Atoi(r.FormValue("batch_month"))
	batchYear, _ := strconv.Atoi(r.FormValue("batch_year"))
	startSeq, _ := strconv.Atoi(r.FormValue("start_sequence"))
	quantity, _ := strconv.Atoi(r.FormValue("quantity"))
	notes := r.FormValue("notes")
	model := product.Normalize(r.FormValue("model"))
	if model != "" && !h.isModelOption(r.Context(), model) {
		http.Error(w, "Unknown model", http.StatusBadRequest)
		return
	}

	if name == "" || productCode == "" || modelCode == "" ||
		batchMonth < 1 || batchMonth > 12 || batchYear < 0 || batchYear > 99 ||
		startSeq < 1 || quantity < 1 {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}
	// The serial schema is a fixed 9-char prefix + 5-digit sequence = 14 chars, and the
	// device-matching queries hard-code LENGTH(serial)=14. Reject any component length
	// that would produce a serial that can never match a device.
	if len(productCode) != 2 || len(modelCode) != 2 || len(variant) != 1 || len(sku) != 2 {
		http.Error(w, "Product code and SKU must be exactly 2 characters, model code 2 digits, variant 1 character.", http.StatusBadRequest)
		return
	}
	// The batch code normally encodes month+year, but the earliest runs shipped with
	// codes that predate that convention (e.g. "26", which decodes to month 0). Those
	// devices exist, so let an admin type the literal code; month/year still record
	// when the run happened.
	batch := strings.ToUpper(strings.TrimSpace(r.FormValue("batch")))
	if batch == "" {
		batch = db.EncodeBatch(batchMonth, batchYear)
	} else if len(batch) != 2 || strings.IndexFunc(batch, func(c rune) bool {
		return !((c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z'))
	}) >= 0 {
		http.Error(w, "Batch code must be exactly 2 letters or digits.", http.StatusBadRequest)
		return
	}

	endSeq := startSeq + quantity - 1
	params := db.ProductionParams{
		Name:          name,
		ProductCode:   productCode,
		ModelCode:     modelCode,
		Variant:       variant,
		SKU:           sku,
		Batch:         batch,
		BatchMonth:    batchMonth,
		BatchYear:     batchYear,
		StartSequence: startSeq,
		EndSequence:   endSeq,
		Notes:         notes,
		Model:         model,
	}
	prod, err := h.db.CreateProduction(r.Context(), params)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/productions/"+prod.ID.String(), http.StatusFound)
}

func (h *Handler) ProductionDetail(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid production ID", http.StatusBadRequest)
		return
	}
	prod, err := h.db.GetProduction(r.Context(), id, h.connectedSlice())
	if err != nil {
		http.Error(w, "Production not found", http.StatusNotFound)
		return
	}
	devices, err := h.db.ListDevices(r.Context(), db.DeviceFilter{ProductionID: id}, 0, 2000, "serial", "asc")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.render(w, r, "production_detail.html", map[string]any{
		"Production":          prod,
		"Models":              h.modelOptions(r.Context()),
		"Devices":             devices,
		"Online":              h.onlineMap(),
		"ActiveThresholdSecs": h.cfg.CheckinInterval() * 3,
	})
}

func (h *Handler) ProductionExportCSV(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid production ID", http.StatusBadRequest)
		return
	}
	prod, err := h.db.GetProduction(r.Context(), id, h.connectedSlice())
	if err != nil {
		http.Error(w, "Production not found", http.StatusNotFound)
		return
	}
	devices, err := h.db.GetProductionDevices(r.Context(), id, h.connectedSlice())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	// Index connected devices by serial for O(1) lookup
	connected := make(map[string]*db.ProductionDevice, len(devices))
	for i := range devices {
		connected[devices[i].Serial] = &devices[i]
	}

	filename := safeCSVFilename(prod.Name, "production")
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))

	cw := csv.NewWriter(w)
	cw.Write([]string{"serial_number", "status", "build_id", "battery_pct", "last_seen_at", "first_seen_at"})

	prefix := prod.SerialPrefix()
	for seq := prod.StartSequence; seq <= prod.EndSequence; seq++ {
		serial := fmt.Sprintf("%s%05d", prefix, seq)
		if dev, ok := connected[serial]; ok {
			lastSeen := ""
			firstSeen := ""
			if !dev.LastSeenAt.IsZero() {
				lastSeen = dev.LastSeenAt.UTC().Format(time.RFC3339)
			}
			if !dev.CreatedAt.IsZero() {
				firstSeen = dev.CreatedAt.UTC().Format(time.RFC3339)
			}
			cw.Write([]string{serial, dev.ConnectionStatus, dev.BuildID, strconv.Itoa(dev.BatteryPct), lastSeen, firstSeen})
		} else {
			cw.Write([]string{serial, "never", "", "", "", ""})
		}
	}
	cw.Flush()
}

func (h *Handler) ProductionDelete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid production ID", http.StatusBadRequest)
		return
	}
	if err := h.db.DeleteProduction(r.Context(), id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/productions", http.StatusFound)
}

// ProductionPreviewSerial returns the first/last serial for the given form values (HTMX partial).
func (h *Handler) ProductionPreviewSerial(w http.ResponseWriter, r *http.Request) {
	productCode := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("product_code")))
	modelCode := strings.TrimSpace(r.URL.Query().Get("model_code"))
	variant := r.URL.Query().Get("variant")
	if variant == "" {
		variant = "0"
	}
	sku := strings.ToUpper(r.URL.Query().Get("sku"))
	if sku == "" {
		sku = "AA"
	}
	batchMonth, _ := strconv.Atoi(r.URL.Query().Get("batch_month"))
	batchYear, _ := strconv.Atoi(r.URL.Query().Get("batch_year"))
	startSeq, _ := strconv.Atoi(r.URL.Query().Get("start_sequence"))
	quantity, _ := strconv.Atoi(r.URL.Query().Get("quantity"))

	if productCode == "" || modelCode == "" || batchMonth < 1 || batchMonth > 12 || batchYear < 0 || startSeq < 1 || quantity < 1 {
		fmt.Fprint(w, `<span class="muted">—</span>`)
		return
	}

	batch := db.EncodeBatch(batchMonth, batchYear)
	prefix := productCode + modelCode + variant + sku + batch
	first := fmt.Sprintf("%s%05d", prefix, startSeq)
	last := fmt.Sprintf("%s%05d", prefix, startSeq+quantity-1)
	fmt.Fprintf(w, `<code class="serial-preview">%s</code> → <code class="serial-preview">%s</code> &nbsp;<span class="muted small">batch <strong>%s</strong></span>`, first, last, batch)
}
