package dashboard

import (
	"mdm/internal/db"
	"mdm/internal/product"
)

// mayManageVisits: who runs Setup (the Ops lead and super admins). The name is from the days
// of setup visits; it is the Setup page's gate and the Ops lead's right to place any device.
func mayManageVisits(role string) bool {
	return role == "admin" || role == "fleet_admin" || role == "fleet_manager"
}

// classOf is the device's class label for setup rows.
func classOf(d db.Device) string { return product.ClassLabel(d.Class()) }
