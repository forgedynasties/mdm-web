package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	_ "image/gif"
	_ "image/jpeg"
	"log"
	"net/http"
	"strings"
	"time"

	"mdm/internal/db"
	"mdm/internal/product"

	"github.com/google/uuid"
)

// RecipeCreate saves the current Actions-builder state as a named recipe that can
// be replayed in one click. It mirrors CommandCreate's field parsing but persists
// the preset instead of dispatching it.
func (h *Handler) RecipeCreate(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	cmdType := r.FormValue("type")
	if name == "" || cmdType == "" {
		http.Error(w, "name and type are required", http.StatusBadRequest)
		return
	}
	if writeCommandAuthzError(w, h.authorizeCommand(h.role(r), cmdType)) {
		return
	}
	// A recipe is stored and shown as it is, so a type that carries a password (a
	// wifi_config) cannot be one.
	if product.CommandHasSecret(cmdType) {
		http.Error(w, cmdTypeLabel(cmdType)+" can't be saved as a recipe: it carries a password.", http.StatusBadRequest)
		return
	}
	targetType := r.FormValue("target_type")
	if targetType != "all" && targetType != "devices" && targetType != "groups" && targetType != "scope" && targetType != "mixed" {
		targetType = "all"
	}
	rec := db.Recipe{
		Name:          name,
		Type:          cmdType,
		ApkURL:        strings.TrimSpace(r.FormValue("apk_url")),
		Payload:       buildPayload(cmdType, r),
		TargetType:    targetType,
		TargetSerials: db.ParseSerials(r.FormValue("target_serials")),
		CreatedBy:     h.role(r),
	}
	for _, g := range r.Form["target_groups"] {
		if id, err := uuid.Parse(g); err == nil {
			rec.TargetGroups = append(rec.TargetGroups, id)
		}
	}
	// Capture the scope-rail selection so a scheduled run can re-resolve it.
	if targetType == "scope" {
		scope, _ := json.Marshal(map[string]string{
			"mode":    r.FormValue("scope_mode"),
			"id":      r.FormValue("scope_id"),
			"status":  r.FormValue("scope_status"),
			"battery": r.FormValue("scope_battery"),
			"kiosk":   r.FormValue("scope_kiosk"),
		})
		rec.Scope = scope
	}
	if _, err := h.db.CreateRecipe(r.Context(), rec); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/commands", http.StatusFound)
}

// RecipeDelete removes a saved recipe.
func (h *Handler) RecipeDelete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid recipe ID", http.StatusBadRequest)
		return
	}
	if err := h.db.DeleteRecipe(r.Context(), id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/commands", http.StatusFound)
}

// ── Scheduled recipes ────────────────────────────────────────────────────────────

// schedulableType reports whether a recipe of this command type can be scheduled.
// logcat and set_kiosk are excluded — they aren't queued commands (they fan out via
// their own mechanisms and their params aren't captured in a recipe payload).
func schedulableType(t string) bool {
	switch t {
	case "install_apk", "uninstall", "reboot", "screenshot", "shell", "update_splash",
		// e.g. a nightly restart to clear WebView memory buildup
		"app_reload", "app_restart", "app_clear_cache":
		return true
	}
	return false
}

// ProcessDueScheduledRecipes fires every schedule whose next run has arrived, then
// advances it (next cron time) or disables it (run-once). Called from a 1-min ticker.
func (h *Handler) ProcessDueScheduledRecipes(ctx context.Context) {
	now := time.Now().UTC()
	due, err := h.db.ListDueScheduledRecipes(ctx, now)
	if err != nil {
		log.Printf("[recipe-scheduler] list due: %v", err)
		return
	}
	for _, s := range due {
		if err := h.fireSchedule(ctx, s, nil); err != nil {
			log.Printf("[recipe-scheduler] fire schedule=%s recipe=%q: %v", s.ID, s.RecipeName, err)
		}
		// Advance: compute the next cron time, or disable a run-once (or unparseable) one.
		var next *time.Time
		disable := s.RunOnce
		if !disable {
			if n, err := db.NextCron(s.CronExpr, now); err == nil {
				n = n.UTC()
				next = &n
			} else {
				disable = true
			}
		}
		if err := h.db.MarkScheduleFired(ctx, s.ID, now, next, disable); err != nil {
			log.Printf("[recipe-scheduler] mark fired %s: %v", s.ID, err)
		}
	}
}

// fireSchedule resolves the recipe's target now and dispatches it as a command.
// fireSchedule runs a schedule's recipe as the person who scheduled it: acc is that
// person's access (nil = look it up from created_by, for the cron), and the targets
// are narrowed to what their policy allows — a schedule must not outlive, or exceed,
// its creator's rights.
func (h *Handler) fireSchedule(ctx context.Context, s db.ScheduledRecipe, acc *access) error {
	if acc == nil {
		var err error
		if acc, err = h.scheduleOwnerAccess(ctx, s.CreatedBy); err != nil {
			return err
		}
	}
	rec, err := h.db.GetRecipe(ctx, s.RecipeID)
	if err != nil {
		return err
	}
	if !schedulableType(rec.Type) {
		return fmt.Errorf("recipe type %q is not schedulable", rec.Type)
	}
	ids, err := h.db.ResolveTargetIDs(ctx, rec.TargetType, rec.TargetSerials, rec.TargetGroups, rec.Scope)
	if err != nil {
		return err
	}
	if h.authorizeCommand(acc.role, rec.Type) != cmdAuthzOK {
		return fmt.Errorf("%s may no longer send %s", s.CreatedBy, rec.Type)
	}
	ids, dropped := acc.filterDevices(policyActionForCommand(rec.Type), ids)
	if dropped > 0 {
		log.Printf("[recipe-scheduler] schedule=%s: %d device(s) outside %s's access policy skipped", s.ID, dropped, s.CreatedBy)
	}
	if len(ids) == 0 {
		log.Printf("[recipe-scheduler] schedule=%s recipe=%q matched 0 devices — skipped", s.ID, rec.Name)
		return nil
	}
	if max := h.cfg.MaxTargets(); max > 0 && len(ids) > max {
		return fmt.Errorf("resolved %d devices exceeds the max-targets limit %d", len(ids), max)
	}
	cmd, err := h.db.CreateCommandBy(ctx, rec.Type, rec.ApkURL, rec.Payload, "devices", ids, "Scheduled recipe: "+rec.Name)
	if err != nil {
		return err
	}
	h.pushCommand(ctx, cmd, "devices", ids)
	log.Printf("[recipe-scheduler] fired schedule=%s recipe=%q type=%s devices=%d", s.ID, rec.Name, rec.Type, len(ids))
	return nil
}

// scheduleOwnerAccess is the access of the account that created a schedule. Older
// rows stored the creator's role rather than a username; only "admin" is honoured
// from those, since a role alone says nothing about which devices are theirs.
func (h *Handler) scheduleOwnerAccess(ctx context.Context, createdBy string) (*access, error) {
	if u, err := h.db.GetUserByUsername(ctx, createdBy); err == nil && u != nil {
		return h.accessFor(ctx, u.Role, u.Username), nil
	}
	if createdBy == "admin" {
		return h.accessFor(ctx, "admin", "admin"), nil
	}
	return nil, fmt.Errorf("schedule creator %q is not an account; recreate the schedule", createdBy)
}
