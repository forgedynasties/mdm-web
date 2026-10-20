package db

import "context"

// DeviceView is the device page a user has chosen: "" (classic, the default) or "new".
func (d *DB) DeviceView(ctx context.Context, username string) (string, error) {
	var v string
	err := d.pool.QueryRow(ctx, `SELECT device_view FROM users WHERE username = $1`, username).Scan(&v)
	return v, err
}

// SetDeviceView records the user's device page choice.
func (d *DB) SetDeviceView(ctx context.Context, username, view string) error {
	_, err := d.pool.Exec(ctx, `UPDATE users SET device_view = $2 WHERE username = $1`, username, view)
	return err
}

// FleetView is how a user's device lists are drawn: "" (the table, the default) or "cards".
func (d *DB) FleetView(ctx context.Context, username string) (string, error) {
	var v string
	err := d.pool.QueryRow(ctx, `SELECT fleet_view FROM users WHERE username = $1`, username).Scan(&v)
	return v, err
}

// SetFleetView records the user's device list choice.
func (d *DB) SetFleetView(ctx context.Context, username, view string) error {
	_, err := d.pool.Exec(ctx, `UPDATE users SET fleet_view = $2 WHERE username = $1`, username, view)
	return err
}
