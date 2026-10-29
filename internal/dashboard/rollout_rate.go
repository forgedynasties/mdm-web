package dashboard

// How fast the minute jobs put work on devices (scalability plan W7, 29 Oct 2026).
//
// The client auto-update, the role app install, role firmware and compliance auto-fix
// each started at most a fixed number of devices a minute (20, or 10 for firmware).
// Right for 200 devices; at 5,000 a client rollout would take four hours a slot whatever
// the server could carry. Now each starts a share of the devices online at the moment,
// with the old fixed number as the floor (a small fleet behaves exactly as before) and a
// ceiling so one tick never queues a flood. 2 % a minute takes a whole fleet in under an
// hour.

const (
	rolloutShare   = 0.02 // of the devices online now, per minute
	rolloutMaxApps = 250  // installs and client updates: small downloads
	rolloutMaxOTA  = 100  // firmware: hundreds of MB each
	rolloutMinApps = 20   // the old fixed caps
	rolloutMinOTA  = 10
)

// rolloutBatch is how many devices one tick of a rollout job may start: share × online,
// at least floor and at most ceiling.
func rolloutBatch(online, floor, ceiling int) int {
	n := int(float64(online)*rolloutShare + 0.999)
	if n < floor {
		n = floor
	}
	if n > ceiling {
		n = ceiling
	}
	return n
}

// appRolloutBatch is the per-tick cap for installs, client updates and auto-fixes.
func (h *Handler) appRolloutBatch() int {
	return rolloutBatch(h.onlineCount(), rolloutMinApps, rolloutMaxApps)
}

// otaRolloutBatch is the per-tick cap for role firmware.
func (h *Handler) otaRolloutBatch() int {
	return rolloutBatch(h.onlineCount(), rolloutMinOTA, rolloutMaxOTA)
}

// onlineCount is the devices with a live socket now (free: the hub's map).
func (h *Handler) onlineCount() int {
	if h.hub == nil {
		return 0
	}
	return len(h.hub.ConnectedIDs())
}
