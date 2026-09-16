package dashboard

import (
	"context"
	"log"
	"time"

	"mdm/internal/otaconfig"
)

// PublishOTAConfig writes ota_config.json to S3 when the rolling reboot window has moved.
// Runs hourly; see internal/otaconfig for why the MDM owns that file.
//
// No-ops unless an admin turned it on: the object is read by every legacy device in the
// field, so only one MDM should ever publish it (a stage instance must not fight the live
// one over the fleet's reboot policy).
func (h *Handler) PublishOTAConfig(ctx context.Context) {
	if h.apk == nil || !h.cfg.OTAConfigManaged() {
		return
	}
	prev, _ := h.cfg.OTAConfigLast()
	opts := h.cfg.OTAConfigOptions()
	// The window is read in each device's OWN local time, so compute it for the devices
	// that are actually taking an update right now. All in one zone: use it. Mixed, or
	// nothing updating: fall back to LA, which is where most of the fleet is.
	if zones, err := h.db.LegacyUpdateTimezones(ctx); err == nil {
		opts.Timezone = otaconfig.ZoneFor(zones)
	}
	body, wrote, err := otaconfig.Publish(ctx, h.apk, opts, time.Now(), []byte(prev))
	if err != nil {
		log.Printf("[ota-config] publish: %v", err)
		return
	}
	if !wrote {
		return
	}
	if err := h.cfg.SetOTAConfigPublished(string(body), time.Now()); err != nil {
		log.Printf("[ota-config] record publish: %v", err)
	}
	start, end := otaconfig.Window(opts, time.Now())
	log.Printf("[ota-config] published %s — reboot window %02d:00-%02d:00 %s",
		otaconfig.Key, start, end, opts.Timezone)
}
