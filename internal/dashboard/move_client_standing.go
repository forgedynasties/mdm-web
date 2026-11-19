package dashboard

import (
	"context"

	"mdm/internal/db"
	"mdm/internal/product"
)

// ClientStandingFor is where a device's MDM client stands against the build hosted for its
// slot (firmware-qcom / firmware-gms / dpc ...), the way the Clients page and the auto-update
// job decide "behind". The device-move watcher (api/device_move_watch.go) gates a move on it.
func (h *Handler) ClientStandingFor(_ context.Context, d *db.Device) db.ClientStanding {
	slot := agentSlotFor(d)
	name, code := clientVersionOf(d)
	st := db.ClientStanding{Slot: slot, Have: name}
	b, hosted := h.cfg.AgentAPKSlot(slot)
	apkName, known := AgentAPKSlots[slot]
	switch {
	case !hosted || b.VersionCode == 0 || !known:
		st.State = "nobuild"
		return st
	case code == 0 || !versionComparable(d, b.Package):
		st.State, st.Want = "unknown", b.Version
		return st
	}
	st.Want = b.Version
	if code >= b.VersionCode {
		st.State = "current"
		return st
	}
	st.State = "behind"
	st.CanUpdate = d.Supports(product.CapSelfUpdate)
	st.URL = h.publicBase() + "/agent/" + apkName
	st.Payload = agentUpdatePayload(b.Package, b.Version, b.SHA256Hex)
	return st
}
