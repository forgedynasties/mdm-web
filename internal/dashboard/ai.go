package dashboard

import (
	"context"
	"encoding/json"
	_ "image/gif"
	_ "image/jpeg"
	"log"
	"net/http"
	"strings"
	"time"

	"mdm/internal/ai"
	"mdm/internal/db"

	"github.com/google/uuid"
)

// SettingsSetAI saves the Anthropic API key, model, and daily-digest toggle. An
// empty key field is treated as "keep the current key" so saving the model/digest
// never clears a configured key; submit the literal "-" to clear it.
func (h *Handler) SettingsSetAI(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	if p := strings.TrimSpace(r.FormValue("ai_provider")); p != "" {
		h.cfg.SetAIProvider(p)
	}
	if k := strings.TrimSpace(r.FormValue("anthropic_api_key")); k != "" {
		if k == "-" {
			k = ""
		}
		h.cfg.SetAnthropicAPIKey(k)
	}
	// Model and base URL are saved verbatim (empty allowed — defaults fill in).
	h.cfg.SetAnthropicModel(strings.TrimSpace(r.FormValue("anthropic_model")))
	h.cfg.SetAIBaseURL(strings.TrimSpace(r.FormValue("ai_base_url")))
	h.cfg.SetAIDigestEnabled(r.FormValue("ai_digest") == "on")
	h.audit(r, "ai.settings", "", "")
	h.settingsRedirect(w, r)
}

// writeAIResult runs an analysis closure under a timeout, records token usage, and
// writes a JSON {"text","model","generated_at","input_tokens","output_tokens"}
// body, or an error JSON on failure.
func (h *Handler) writeAIResult(w http.ResponseWriter, r *http.Request, run func(ctx context.Context, c *ai.Client) (string, ai.Usage, error)) {
	if !h.cfg.AIEnabled() {
		writeJSONError(w, http.StatusServiceUnavailable, "AI analysis is not configured — add an Anthropic API key in Settings.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	client := ai.New(h.cfg.AIProvider(), h.cfg.AnthropicAPIKey(), h.cfg.AnthropicModel(), h.cfg.AIBaseURL())
	text, usage, err := run(ctx, client)
	if err != nil {
		log.Printf("[ai] analysis failed: %v", err)
		writeJSONError(w, http.StatusBadGateway, "Analysis failed: "+err.Error())
		return
	}
	if err := h.db.RecordAIUsage(ctx, usage.InputTokens, usage.OutputTokens); err != nil {
		log.Printf("[ai] record usage: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"text":          text,
		"model":         client.Model(),
		"generated_at":  time.Now().Format(time.RFC3339),
		"input_tokens":  usage.InputTokens,
		"output_tokens": usage.OutputTokens,
	})
}

// DeviceAIAnalysis returns an AI reading of one device's recent trends + open alerts.
func (h *Handler) DeviceAIAnalysis(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "Device not found")
		return
	}
	var resultText, resultModel string
	h.writeAIResult(w, r, func(ctx context.Context, c *ai.Client) (string, ai.Usage, error) {
		stats, err := h.db.GetDeviceDailyStats(ctx, device.ID, 30)
		if err != nil {
			return "", ai.Usage{}, err
		}
		// No per-device alert query exists; filter the open list by serial.
		open, _ := h.db.ListAlerts(ctx, "open", 200)
		var devAlerts []db.Alert
		for _, a := range open {
			if a.Serial == serial {
				devAlerts = append(devAlerts, a)
			}
		}
		text, usage, err := c.AnalyzeDevice(ctx, serial, device.DeployedEffective, stats, devAlerts)
		if err == nil {
			resultText, resultModel = text, c.Model()
		}
		return text, usage, err
	})
	if resultText != "" {
		if err := h.db.SaveDeviceAIAnalysis(r.Context(), device.ID, resultModel, resultText); err != nil {
			log.Printf("[ai] save device analysis: %v", err)
		}
	}
	h.audit(r, "ai.device", serial, "")
}

// AlertLogcatAnalyze runs an AI triage over the logs auto-captured for a crash
// alert and returns the analysis as JSON (same shape as the other AI endpoints).
func (h *Handler) AlertLogcatAnalyze(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid alert id")
		return
	}
	if acc := h.access(r); acc.hidesDevices() {
		if dev, err := h.db.AlertDeviceID(r.Context(), id); err != nil || (dev != nil && !acc.visible(*dev)) {
			writeJSONError(w, http.StatusNotFound, "No captured logs to analyze yet.")
			return
		}
	}
	lc, ok, err := h.db.GetAlertLogcat(r.Context(), id)
	if err != nil || !ok || strings.TrimSpace(lc.Content) == "" {
		writeJSONError(w, http.StatusNotFound, "No captured logs to analyze yet.")
		return
	}
	h.writeAIResult(w, r, func(ctx context.Context, c *ai.Client) (string, ai.Usage, error) {
		return c.AnalyzeLog(ctx, lc.Content)
	})
	h.audit(r, "ai.logcat.analyze", id.String(), "")
}

// DeviceAIAnalysesList returns a device's stored AI analyses (newest first) as JSON.
func (h *Handler) DeviceAIAnalysesList(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	device, err := h.db.GetDevice(r.Context(), serial)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "Device not found")
		return
	}
	list, err := h.db.ListDeviceAIAnalyses(r.Context(), device.ID, 20)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Failed to load analyses")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"analyses": list})
}
