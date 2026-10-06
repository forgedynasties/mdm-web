package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// hwSeen remembers, per AIO serial, the chip serial already stored this run, so a check-in
// that repeats it costs nothing. After a restart each device writes once.
var hwSeen sync.Map // serial -> string

// noteHardwareSerial stores the chip serial a device reports and alerts when it does not fit
// what is known. It runs off the check-in's path and never refuses a check-in.
func (h *Handler) noteHardwareSerial(ctx context.Context, deviceID uuid.UUID, serial, hw string) {
	if v, ok := hwSeen.Load(serial); ok && v.(string) == hw {
		return
	}
	hwSeen.Store(serial, hw)
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		ev, err := h.db.RecordHardwareSerial(ctx, deviceID, serial, hw)
		if err != nil {
			hwSeen.Delete(serial) // try again on the next check-in
			log.Printf("[hw-serial] %s: %v", serial, err)
			return
		}
		var summary string
		switch ev.Kind {
		case "board_changed":
			summary = fmt.Sprintf("Hardware serial changed: %s now reports %s, it reported %s before", serial, hw, ev.Previous)
		case "serial_changed":
			summary = fmt.Sprintf("Hardware serial %s was also seen as %s: the AIO serial may have changed or been copied", hw, strings.Join(ev.Others, ", "))
		default:
			return
		}
		created, err := h.db.CreateAlertIfAbsent(ctx, nil, "hardware_serial_changed", deviceID, "warning", summary,
			map[string]any{"hardware_serial": hw, "previous": ev.Previous, "also_seen_as": ev.Others})
		if err != nil {
			log.Printf("[hw-serial] alert for %s: %v", serial, err)
			return
		}
		if created {
			h.alerts.Dispatch(ctx, []db.AlertNotification{{Type: "hardware_serial_changed", Severity: "warning", Summary: summary,
				Serial: serial, DeviceID: deviceID, EventAt: time.Now().UTC()}})
		}
		h.hub.PublishAlertUpdate()
	}()
}

// ListHardwareSerials is the map from chip serial to AIO serial (admin key).
// GET /api/v1/hardware-serials?hw=C45BCE30&serial=AT070AA2600030, both optional.
func (h *Handler) ListHardwareSerials(w http.ResponseWriter, r *http.Request) {
	hw := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("hw")))
	rows, err := h.db.ListHardwareSerials(r.Context(), hw, strings.TrimSpace(r.URL.Query().Get("serial")))
	if err != nil {
		log.Printf("[hw-serial] list: %v", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"hardware_serials": rows})
}
