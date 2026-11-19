package db

import (
	"context"
	"encoding/json"
)

// MoveClientDevices reads, for the Moves page, just enough of each named device (hidden ones
// too) to measure its MDM client against the hosted build: kind, product, build and the
// version keys of its latest check-in. A serial with no device is left out.
func (d *DB) MoveClientDevices(ctx context.Context, serials []string) (map[string]Device, error) {
	out := map[string]Device{}
	if len(serials) == 0 {
		return out, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT serial_number, COALESCE(build_id,''), product, agent_kind, device_class,
		       jsonb_strip_nulls(jsonb_build_object(
		         'agent_version', latest_extra->'agent_version',
		         'agent_version_code', latest_extra->'agent_version_code',
		         'agent_package', latest_extra->'agent_package',
		         'build_type', latest_extra->'build_type'))
		  FROM devices WHERE serial_number = ANY($1)`, serials)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var dev Device
		var extra []byte
		if err := rows.Scan(&dev.SerialNumber, &dev.BuildID, &dev.Product, &dev.AgentKind, &dev.DeviceClass, &extra); err != nil {
			return nil, err
		}
		dev.LatestExtra = json.RawMessage(extra)
		out[dev.SerialNumber] = dev
	}
	return out, rows.Err()
}
