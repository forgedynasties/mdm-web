package db

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ProductionDevice is a device row augmented with connection status for production detail view.
type ProductionDevice struct {
	Serial           string    `json:"serial"`
	DeviceID         uuid.UUID `json:"device_id,omitempty"`
	BuildID          string    `json:"build_id,omitempty"`
	BatteryPct       int       `json:"battery_pct,omitempty"`
	LastSeenAt       time.Time `json:"last_seen_at,omitempty"`
	CreatedAt        time.Time `json:"created_at,omitempty"`
	ConnectionStatus string    `json:"connection_status"` // "online", "offline", "never"
}

// ── Productions ───────────────────────────────────────────────────────────────

type ProductionParams struct {
	Name          string
	ProductCode   string
	ModelCode     string
	Variant       string
	SKU           string
	Batch         string
	BatchMonth    int
	BatchYear     int
	StartSequence int
	EndSequence   int
	Notes         string
	Model         string
}

func (d *DB) CreateProduction(ctx context.Context, p ProductionParams) (*Production, error) {
	var prod Production
	err := d.pool.QueryRow(ctx, `
		INSERT INTO productions (name, product_code, model_code, variant, sku, batch, batch_month, batch_year, start_sequence, end_sequence, notes, model)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING id, name, product_code, model_code, variant, sku, batch, batch_month, batch_year, start_sequence, end_sequence, notes, model, created_at
	`, p.Name, p.ProductCode, p.ModelCode, p.Variant, p.SKU, p.Batch, p.BatchMonth, p.BatchYear, p.StartSequence, p.EndSequence, p.Notes, p.Model).
		Scan(&prod.ID, &prod.Name, &prod.ProductCode, &prod.ModelCode, &prod.Variant, &prod.SKU,
			&prod.Batch, &prod.BatchMonth, &prod.BatchYear, &prod.StartSequence, &prod.EndSequence, &prod.Notes, &prod.Model, &prod.CreatedAt)
	if err != nil {
		return nil, err
	}
	prod.Total = prod.EndSequence - prod.StartSequence + 1
	return &prod, nil
}

// ListProductions returns productions with per-batch counts. "online" is devices with
// a live WebSocket (from the connected set), matching the WS-based status elsewhere.
func (d *DB) ListProductions(ctx context.Context, connected []uuid.UUID) ([]Production, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT
			p.id, p.name, p.product_code, p.model_code, p.variant, p.sku, p.batch,
			p.batch_month, p.batch_year, p.start_sequence, p.end_sequence, p.notes, p.model, p.created_at,
			p.end_sequence - p.start_sequence + 1 AS total,
			COUNT(d.id) AS ever_connected,
			COUNT(d.id) FILTER (WHERE d.id = ANY($1::uuid[])) AS online
		FROM productions p
		LEFT JOIN devices d ON
			d.serial_number LIKE (p.product_code || p.model_code || p.variant || p.sku || p.batch || '%')
			AND LENGTH(d.serial_number) = 14
			AND SUBSTRING(d.serial_number FROM 10 FOR 5) ~ '^[0-9]+$'
			AND CAST(SUBSTRING(d.serial_number FROM 10 FOR 5) AS INT) BETWEEN p.start_sequence AND p.end_sequence
		GROUP BY p.id
		ORDER BY p.created_at DESC
	`, connected)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var productions []Production
	for rows.Next() {
		var p Production
		if err := rows.Scan(&p.ID, &p.Name, &p.ProductCode, &p.ModelCode, &p.Variant, &p.SKU, &p.Batch,
			&p.BatchMonth, &p.BatchYear, &p.StartSequence, &p.EndSequence, &p.Notes, &p.Model, &p.CreatedAt,
			&p.Total, &p.EverConnected, &p.Online); err != nil {
			return nil, err
		}
		productions = append(productions, p)
	}
	return productions, rows.Err()
}

func (d *DB) GetProduction(ctx context.Context, id uuid.UUID, connected []uuid.UUID) (*Production, error) {
	var p Production
	err := d.pool.QueryRow(ctx, `
		SELECT
			p.id, p.name, p.product_code, p.model_code, p.variant, p.sku, p.batch,
			p.batch_month, p.batch_year, p.start_sequence, p.end_sequence, p.notes, p.model, p.created_at,
			p.end_sequence - p.start_sequence + 1 AS total,
			COUNT(d.id) AS ever_connected,
			COUNT(d.id) FILTER (WHERE d.id = ANY($2::uuid[])) AS online
		FROM productions p
		LEFT JOIN devices d ON
			d.serial_number LIKE (p.product_code || p.model_code || p.variant || p.sku || p.batch || '%')
			AND LENGTH(d.serial_number) = 14
			AND SUBSTRING(d.serial_number FROM 10 FOR 5) ~ '^[0-9]+$'
			AND CAST(SUBSTRING(d.serial_number FROM 10 FOR 5) AS INT) BETWEEN p.start_sequence AND p.end_sequence
		WHERE p.id = $1
		GROUP BY p.id
	`, id, connected).Scan(&p.ID, &p.Name, &p.ProductCode, &p.ModelCode, &p.Variant, &p.SKU, &p.Batch,
		&p.BatchMonth, &p.BatchYear, &p.StartSequence, &p.EndSequence, &p.Notes, &p.Model, &p.CreatedAt,
		&p.Total, &p.EverConnected, &p.Online)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// SetProductionModel links a production run to the hardware model it built ("" unlinks).
func (d *DB) SetProductionModel(ctx context.Context, id uuid.UUID, model string) error {
	_, err := d.pool.Exec(ctx, `UPDATE productions SET model = $2 WHERE id = $1`, id, model)
	return err
}

func (d *DB) DeleteProduction(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM productions WHERE id = $1`, id)
	return err
}

// GetProductionDevices returns connected devices for a production.
// Devices that have never connected are not in this list but can be inferred from total - ever_connected.
func (d *DB) GetProductionDevices(ctx context.Context, id uuid.UUID, connected []uuid.UUID) ([]ProductionDevice, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT
			d.serial_number,
			d.id,
			d.build_id,
			d.latest_battery_pct,
			d.last_seen_at,
			d.created_at,
			CASE
				WHEN d.id = ANY($2::uuid[]) THEN 'online'
				ELSE 'offline'
			END AS connection_status
		FROM productions p
		JOIN devices d ON
			d.serial_number LIKE (p.product_code || p.model_code || p.variant || p.sku || p.batch || '%')
			AND LENGTH(d.serial_number) = 14
			AND SUBSTRING(d.serial_number FROM 10 FOR 5) ~ '^[0-9]+$'
			AND CAST(SUBSTRING(d.serial_number FROM 10 FOR 5) AS INT) BETWEEN p.start_sequence AND p.end_sequence
		WHERE p.id = $1 AND NOT d.hidden
		ORDER BY d.serial_number
	`, id, connected)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var devices []ProductionDevice
	for rows.Next() {
		var dev ProductionDevice
		if err := rows.Scan(&dev.Serial, &dev.DeviceID, &dev.BuildID, &dev.BatteryPct, &dev.LastSeenAt, &dev.CreatedAt, &dev.ConnectionStatus); err != nil {
			return nil, err
		}
		devices = append(devices, dev)
	}
	return devices, rows.Err()
}
