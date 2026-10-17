package db

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AIUsageDay is one day of AI token usage. Used for the Settings usage chart.
type AIUsageDay struct {
	Day          time.Time `json:"day"`
	Calls        int       `json:"calls"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
}

// AIUsageTotals is the all-time AI usage rollup for the Settings summary.
type AIUsageTotals struct {
	Calls        int   `json:"calls"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// DeviceAIAnalysis is one stored AI reading of a device.
type DeviceAIAnalysis struct {
	ID        int64     `json:"id"`
	Model     string    `json:"model"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

// SaveDeviceAIAnalysis records one analysis; old rows beyond the most recent 20
// per device are trimmed so the history stays small.
func (d *DB) SaveDeviceAIAnalysis(ctx context.Context, deviceID uuid.UUID, model, text string) error {
	if _, err := d.pool.Exec(ctx,
		`INSERT INTO device_ai_analyses (device_id, model, text) VALUES ($1, $2, $3)`,
		deviceID, model, text); err != nil {
		return err
	}
	_, _ = d.pool.Exec(ctx, `
		DELETE FROM device_ai_analyses
		WHERE device_id = $1 AND id NOT IN (
			SELECT id FROM device_ai_analyses WHERE device_id = $1 ORDER BY created_at DESC LIMIT 20
		)`, deviceID)
	return nil
}

// ListDeviceAIAnalyses returns a device's recent analyses, newest first.
func (d *DB) ListDeviceAIAnalyses(ctx context.Context, deviceID uuid.UUID, limit int) ([]DeviceAIAnalysis, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := d.pool.Query(ctx,
		`SELECT id, model, text, created_at FROM device_ai_analyses
		 WHERE device_id = $1 ORDER BY created_at DESC LIMIT $2`, deviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceAIAnalysis
	for rows.Next() {
		var a DeviceAIAnalysis
		if err := rows.Scan(&a.ID, &a.Model, &a.Text, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RecordAIUsage adds one call's token counts to today's ai_usage row (upsert).
func (d *DB) RecordAIUsage(ctx context.Context, inputTokens, outputTokens int64) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO ai_usage (day, calls, input_tokens, output_tokens)
		VALUES (CURRENT_DATE, 1, $1, $2)
		ON CONFLICT (day) DO UPDATE SET
			calls         = ai_usage.calls + 1,
			input_tokens  = ai_usage.input_tokens + EXCLUDED.input_tokens,
			output_tokens = ai_usage.output_tokens + EXCLUDED.output_tokens
	`, inputTokens, outputTokens)
	return err
}

// GetAIUsageDaily returns per-day usage for the last `days` days, oldest first.
func (d *DB) GetAIUsageDaily(ctx context.Context, days int) ([]AIUsageDay, error) {
	if days <= 0 {
		days = 30
	}
	rows, err := d.pool.Query(ctx, `
		SELECT day, calls, input_tokens, output_tokens
		FROM ai_usage
		WHERE day >= CURRENT_DATE - ($1::int - 1)
		ORDER BY day`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AIUsageDay
	for rows.Next() {
		var u AIUsageDay
		if err := rows.Scan(&u.Day, &u.Calls, &u.InputTokens, &u.OutputTokens); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// GetAIUsageTotals returns the all-time AI usage rollup.
func (d *DB) GetAIUsageTotals(ctx context.Context) (AIUsageTotals, error) {
	var t AIUsageTotals
	err := d.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(calls),0), COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0)
		FROM ai_usage`).Scan(&t.Calls, &t.InputTokens, &t.OutputTokens)
	return t, err
}

// AISummary is a cached AI summary for one scope.
type AISummary struct {
	Summary     string    `json:"summary"`
	Model       string    `json:"model"`
	GeneratedAt time.Time `json:"generated_at"`
}

// SetAISummary upserts the cached summary for a scope (e.g. "fleet").
func (d *DB) SetAISummary(ctx context.Context, scope, summary, model string) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO ai_summary (scope, summary, model, generated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (scope) DO UPDATE SET
			summary = EXCLUDED.summary, model = EXCLUDED.model, generated_at = NOW()
	`, scope, summary, model)
	return err
}

// GetAISummary returns the cached summary for a scope. A missing row yields a zero
// AISummary and a nil error (no summary generated yet).
func (d *DB) GetAISummary(ctx context.Context, scope string) (AISummary, error) {
	var s AISummary
	err := d.pool.QueryRow(ctx, `
		SELECT summary, model, generated_at FROM ai_summary WHERE scope = $1
	`, scope).Scan(&s.Summary, &s.Model, &s.GeneratedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AISummary{}, nil
	}
	return s, err
}
