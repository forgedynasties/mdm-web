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
	body, wrote, err := otaconfig.Publish(ctx, h.apk, h.cfg.OTAConfigOptions(), time.Now(), []byte(prev))
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
	start, end := otaconfig.Window(h.cfg.OTAConfigOptions(), time.Now())
	log.Printf("[ota-config] published %s — reboot window %02d:00-%02d:00 %s",
		otaconfig.Key, start, end, h.cfg.OTAConfigOptions().Timezone)
}
