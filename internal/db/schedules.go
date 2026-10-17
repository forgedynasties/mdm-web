package db

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// ── Command recipes ─────────────────────────────────────────────────────────────

// Recipe is a saved action preset the Actions builder can replay in one click.
type Recipe struct {
	ID            uuid.UUID       `json:"id"`
	Name          string          `json:"name"`
	Type          string          `json:"type"`
	ApkURL        string          `json:"apk_url"`
	Payload       json.RawMessage `json:"payload"`
	TargetType    string          `json:"target_type"`
	TargetSerials []string        `json:"target_serials"`
	TargetGroups  []uuid.UUID     `json:"target_groups"`
	Scope         json.RawMessage `json:"scope"` // {mode,id,status,battery,kiosk} when target_type='scope'
	CreatedBy     string          `json:"created_by"`
	CreatedAt     time.Time       `json:"created_at"`
}

func (d *DB) CreateRecipe(ctx context.Context, rec Recipe) (*Recipe, error) {
	if rec.Payload == nil {
		rec.Payload = json.RawMessage("{}")
	}
	if rec.Scope == nil {
		rec.Scope = json.RawMessage("{}")
	}
	if rec.TargetSerials == nil {
		rec.TargetSerials = []string{}
	}
	if rec.TargetGroups == nil {
		rec.TargetGroups = []uuid.UUID{}
	}
	err := d.pool.QueryRow(ctx, `
		INSERT INTO command_recipes (name, type, apk_url, payload, target_type, target_serials, target_groups, scope, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING id, created_at`,
		rec.Name, rec.Type, rec.ApkURL, rec.Payload, rec.TargetType, rec.TargetSerials, rec.TargetGroups, rec.Scope, rec.CreatedBy,
	).Scan(&rec.ID, &rec.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

const recipeCols = `id, name, type, apk_url, payload, target_type, target_serials, target_groups, scope, created_by, created_at`

func scanRecipe(row interface{ Scan(...any) error }, rec *Recipe) error {
	return row.Scan(&rec.ID, &rec.Name, &rec.Type, &rec.ApkURL, &rec.Payload, &rec.TargetType, &rec.TargetSerials, &rec.TargetGroups, &rec.Scope, &rec.CreatedBy, &rec.CreatedAt)
}

func (d *DB) ListRecipes(ctx context.Context) ([]Recipe, error) {
	rows, err := d.pool.Query(ctx, `SELECT `+recipeCols+` FROM command_recipes ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Recipe
	for rows.Next() {
		var rec Recipe
		if err := scanRecipe(rows, &rec); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// GetRecipe fetches one recipe by id.
func (d *DB) GetRecipe(ctx context.Context, id uuid.UUID) (*Recipe, error) {
	var rec Recipe
	if err := scanRecipe(d.pool.QueryRow(ctx, `SELECT `+recipeCols+` FROM command_recipes WHERE id = $1`, id), &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// ResolveTargetIDs resolves a stored target spec to current device IDs (request-free),
// used by the scheduler. all → whole fleet, devices → serials, groups → current members,
// scope → the DeviceFilter that the scope rail builds (re-resolved now).
func (d *DB) ResolveTargetIDs(ctx context.Context, targetType string, serials []string, groups []uuid.UUID, scope json.RawMessage) ([]uuid.UUID, error) {
	switch targetType {
	case "devices":
		return d.GetDeviceIDsBySerials(ctx, serials)
	case "groups":
		return d.GetDeviceIDsInGroups(ctx, groups)
	case "scope":
		var s struct {
			Mode, ID, Status, Battery, Kiosk string
		}
		_ = json.Unmarshal(scope, &s)
		f := DeviceFilter{ActiveThresholdSecs: 180, Online: s.Status, Battery: s.Battery, Kiosk: s.Kiosk}
		switch s.Mode {
		case "restaurant":
			if id, err := uuid.Parse(s.ID); err == nil {
				f.RestaurantID = id
			}
		case "group":
			if id, err := uuid.Parse(s.ID); err == nil {
				f.GroupID = id
			}
		case "build":
			f.BuildID = s.ID
		}
		devs, err := d.ListDevices(ctx, f, 0, 20000, "serial", "asc")
		if err != nil {
			return nil, err
		}
		ids := make([]uuid.UUID, len(devs))
		for i := range devs {
			ids[i] = devs[i].ID
		}
		return ids, nil
	default: // "all"
		return d.GetAllDeviceIDs(ctx)
	}
}

func (d *DB) DeleteRecipe(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM command_recipes WHERE id = $1`, id)
	return err
}

// ── Scheduled recipes ────────────────────────────────────────────────────────────

// ScheduledRecipe runs a recipe on a cron; RecipeName/RecipeType are joined for display.
type ScheduledRecipe struct {
	ID         uuid.UUID  `json:"id"`
	RecipeID   uuid.UUID  `json:"recipe_id"`
	RecipeName string     `json:"recipe_name"`
	RecipeType string     `json:"recipe_type"`
	CronExpr   string     `json:"cron_expr"`
	Enabled    bool       `json:"enabled"`
	RunOnce    bool       `json:"run_once"`
	NextRunAt  *time.Time `json:"next_run_at"`
	LastRunAt  *time.Time `json:"last_run_at"`
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
}

func (d *DB) CreateScheduledRecipe(ctx context.Context, recipeID uuid.UUID, cronExpr string, runOnce bool, nextRun time.Time, createdBy string) (*ScheduledRecipe, error) {
	var s ScheduledRecipe
	err := d.pool.QueryRow(ctx, `
		INSERT INTO scheduled_recipes (recipe_id, cron_expr, run_once, next_run_at, created_by)
		VALUES ($1,$2,$3,$4,$5) RETURNING id, created_at`,
		recipeID, cronExpr, runOnce, nextRun, createdBy).Scan(&s.ID, &s.CreatedAt)
	if err != nil {
		return nil, err
	}
	s.RecipeID, s.CronExpr, s.RunOnce, s.Enabled = recipeID, cronExpr, runOnce, true
	s.NextRunAt = &nextRun
	return &s, nil
}

const schedCols = `s.id, s.recipe_id, r.name, r.type, s.cron_expr, s.enabled, s.run_once, s.next_run_at, s.last_run_at, s.created_by, s.created_at`

func scanSchedule(row interface{ Scan(...any) error }, s *ScheduledRecipe) error {
	return row.Scan(&s.ID, &s.RecipeID, &s.RecipeName, &s.RecipeType, &s.CronExpr, &s.Enabled, &s.RunOnce, &s.NextRunAt, &s.LastRunAt, &s.CreatedBy, &s.CreatedAt)
}

func (d *DB) ListScheduledRecipes(ctx context.Context) ([]ScheduledRecipe, error) {
	rows, err := d.pool.Query(ctx, `SELECT `+schedCols+`
		FROM scheduled_recipes s JOIN command_recipes r ON r.id = s.recipe_id
		ORDER BY s.enabled DESC, s.next_run_at NULLS LAST, s.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScheduledRecipe
	for rows.Next() {
		var s ScheduledRecipe
		if err := scanSchedule(rows, &s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListDueScheduledRecipes returns enabled schedules whose next run has arrived.
func (d *DB) ListDueScheduledRecipes(ctx context.Context, now time.Time) ([]ScheduledRecipe, error) {
	rows, err := d.pool.Query(ctx, `SELECT `+schedCols+`
		FROM scheduled_recipes s JOIN command_recipes r ON r.id = s.recipe_id
		WHERE s.enabled AND s.next_run_at IS NOT NULL AND s.next_run_at <= $1`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScheduledRecipe
	for rows.Next() {
		var s ScheduledRecipe
		if err := scanSchedule(rows, &s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (d *DB) DeleteScheduledRecipe(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM scheduled_recipes WHERE id = $1`, id)
	return err
}

// SetScheduleEnabled toggles a schedule; nextRun is set when enabling (nil to clear).
func (d *DB) SetScheduleEnabled(ctx context.Context, id uuid.UUID, enabled bool, nextRun *time.Time) error {
	_, err := d.pool.Exec(ctx, `UPDATE scheduled_recipes SET enabled=$2, next_run_at=$3 WHERE id=$1`, id, enabled, nextRun)
	return err
}

// MarkScheduleFired records a run: set last_run and either the next run time or, for a
// run-once schedule, disable it (next_run NULL).
func (d *DB) MarkScheduleFired(ctx context.Context, id uuid.UUID, ranAt time.Time, nextRun *time.Time, disable bool) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE scheduled_recipes SET last_run_at=$2, next_run_at=$3, enabled = enabled AND NOT $4 WHERE id=$1`,
		id, ranAt, nextRun, disable)
	return err
}
