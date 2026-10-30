package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"mdm/internal/db"
	"mdm/internal/product"
)

// One model, several roles (29 Oct 2026; picked U2 · L1 · D1 · E1 · B1). The Samsung Galaxy
// Tab A9+ is a POS terminal at one counter and a kitchen display in the kitchen, so a model
// can be attached to several roles on Lineup. A device of such a model has no role until
// someone picks one (U2): its role app and lock wait for that, because both follow the
// role. It is picked in AIO Enroll (E1), or on the device page, where Needs a look asks
// and the Device card keeps a picker (D1). The AIO backend's device type is only a
// suggestion a person confirms (B1).

// Per serial (30 Oct 2026; user: "allow engineers to change the class of each device, we can
// allow this for devices other than firmware devices, but per serial not per model"): a
// Standard-client device's Role picker offers every class, and the pick is pinned to that
// serial (class_source "pin", db.PinDeviceClass) so its model's role no longer moves it;
// "Back to the model's role" unpins. Firmware devices keep their product's class.

// deviceRole is what the device page knows about a device's role.
type deviceRole struct {
	Roles     []string // the model's roles, when it has several (nil otherwise)
	Source    string   // devices.class_source: "" model, "set", "enroll", "backend", "pin"
	Backend   string   // the class the AIO backend's device type means, "" none
	Where     string   // the backend restaurant it is listed at
	PerSerial bool     // a Standard-client device: any class, pinned to its serial
}

// Picker reports whether the Device card offers a role picker.
func (r deviceRole) Picker() bool { return r.PerSerial || len(r.Roles) > 1 }

// Choices are the picker's classes: per serial every class a Standard-client device can
// be (perSerialClass), else the model's roles.
func (r deviceRole) Choices() []string {
	if r.PerSerial {
		var out []string
		for _, c := range product.Classes() {
			if perSerialClass(c) {
				out = append(out, c)
			}
		}
		return out
	}
	return r.Roles
}

// perSerialClass: a class a Standard-client device may be set to by serial. Not Tableside
// AI (user, 30 Oct 2026: "changing to tsai not allowed"): a T7 is our own firmware tablet.
func perSerialClass(c string) bool { return product.IsClass(c) && c != product.ClassT7 }

// Pinned reports whether the class was set for this serial.
func (r deviceRole) Pinned() bool { return r.Source == "pin" }

// SourceText is how the Device card says where the role came from.
func (r deviceRole) SourceText() string {
	switch r.Source {
	case "set":
		return "picked on this page"
	case "enroll":
		return "picked in AIO Enroll"
	case "backend":
		return "from the AIO backend"
	case "pin":
		return "set for this device"
	}
	return "from the model"
}

func (h *Handler) deviceRoleOf(ctx context.Context, d *db.Device) deviceRole {
	var out deviceRole
	if d.AgentKind != product.KindFirmware {
		out.PerSerial = true
		out.Source, _ = h.db.DeviceClassSource(ctx, d.SerialNumber)
	}
	if d.Product == "" || product.IsKnown(d.Product) {
		return out
	}
	roles, _ := h.db.ProductRoles(ctx, d.Product)
	if len(roles) < 2 {
		return out
	}
	out.Roles = roles
	out.Source, _ = h.db.DeviceClassSource(ctx, d.SerialNumber)
	if bds, err := h.db.BackendDevicesFor(ctx, deviceBackendIDs(d)); err == nil {
		for _, b := range bds {
			if c := backendTypeClass(b.Type); slices.Contains(roles, c) {
				out.Backend = c
				if r, ok, _ := h.db.BackendRestaurantOf(ctx, b.Env, b.RestaurantID); ok {
					out.Where = r.Name
				}
				break
			}
		}
	}
	return out
}

// roleNeeds are the device's role rows for Needs a look: the question while it has no
// role, and the backend's suggestion when it says otherwise.
func roleNeeds(dr deviceRole, d *db.Device) []deviceNeed {
	if len(dr.Roles) < 2 || dr.Pinned() {
		return nil
	}
	labels := make([]string, len(dr.Roles))
	for i, r := range dr.Roles {
		labels[i] = product.ClassLabel(r)
	}
	if d.DeviceClass == "" {
		n := deviceNeed{Tone: "warn", Action: "role",
			Title: "Is this a " + strings.Join(labels, " or a ") + "?",
			Why:   fmt.Sprintf("%s can be either. Its app and lock wait for the answer.", productLabel(d.Product))}
		for _, r := range dr.Roles {
			n.Choices = append(n.Choices, roleChoice{Key: r, Label: product.ClassLabel(r), Primary: r == dr.Backend})
		}
		if dr.Backend != "" {
			n.Why = fmt.Sprintf("%s can be either; the AIO backend lists it as a %s%s. Its app and lock wait for the answer.",
				productLabel(d.Product), product.ClassLabel(dr.Backend), atWhere(dr.Where))
		}
		return []deviceNeed{n}
	}
	if dr.Backend != "" && dr.Backend != d.DeviceClass {
		return []deviceNeed{{Tone: "info", Action: "role",
			Title:   "The AIO backend lists it as a " + product.ClassLabel(dr.Backend),
			Why:     fmt.Sprintf("It is a %s here%s.", product.ClassLabel(d.DeviceClass), atWhere(dr.Where)),
			Choices: []roleChoice{{Key: dr.Backend, Label: "Make it a " + product.ClassLabel(dr.Backend), Primary: true}}}}
	}
	return nil
}

func atWhere(where string) string {
	if where == "" {
		return ""
	}
	return " at " + where
}

// roleChoice is one button on a role row.
type roleChoice struct {
	Key, Label string
	Primary    bool
}

// DeviceSetRole: POST /devices/{serial}/role {class}. A Standard-client device takes any
// class, pinned to its serial ("model" unpins it); a firmware device only one of its
// several-role model's roles. The role app and lock follow on their own
// (RunRoleAppInstalls next minute, the lock with the next config).
func (h *Handler) DeviceSetRole(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	back := "/devices/" + serial
	class := strings.TrimSpace(r.FormValue("class"))
	d, err := h.db.GetDevice(r.Context(), serial)
	if err != nil || d == nil {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	// An engineer only looks at a venue's devices (stateDecision); the route's own check
	// lets a "notes" action through, so the venue is checked here.
	if s, ok := h.currentSession(r); ok && s.Role == "engineer" && d.RestaurantID != nil {
		h.hxDoneToast(w, r, back, "Read-only at a venue", "error")
		return
	}
	if d.AgentKind != product.KindFirmware {
		if class == "model" {
			if err := h.db.UnpinDeviceClass(r.Context(), serial); err != nil {
				http.Error(w, "Internal error", http.StatusInternalServerError)
				return
			}
			h.audit(r, "device.role", serial, d.DeviceClass+" → the model's role")
			h.hub.PublishDeviceUpdate(d.ID)
			h.hxDoneToast(w, r, back, "Back to its model's role", "success")
			return
		}
		if !perSerialClass(class) {
			h.hxDoneToast(w, r, back, "That role can't be set on this device", "error")
			return
		}
		if err := h.db.PinDeviceClass(r.Context(), serial, class); err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		h.audit(r, "device.role", serial, d.DeviceClass+" → "+class+" (this device)")
		h.hub.PublishDeviceUpdate(d.ID)
		h.hxDoneToast(w, r, back, "Now a "+product.ClassLabel(class)+" · its app and lock follow", "success")
		return
	}
	if d.DeviceClass == class {
		h.hxDoneToast(w, r, back, "It is a "+product.ClassLabel(class)+" already", "success")
		return
	}
	if err := h.db.SetDeviceClass(r.Context(), serial, class, "set"); err != nil {
		if errors.Is(err, db.ErrRoleNotAllowed) {
			h.hxDoneToast(w, r, back, "That role is not one of this model's roles", "error")
			return
		}
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.audit(r, "device.role", serial, d.DeviceClass+" → "+class)
	h.hub.PublishDeviceUpdate(d.ID)
	h.hxDoneToast(w, r, back, "Now a "+product.ClassLabel(class)+" · its app and lock follow", "success")
}

// AppModelRoles: GET /api/v1/app/model-roles?product=<ro.product.name> →
// {"product","roles":[{"key","label"}]}. AIO Enroll asks which one only when there are two
// or more.
func (h *Handler) AppModelRoles(w http.ResponseWriter, r *http.Request, _ *db.Session) {
	key := product.Normalize(r.URL.Query().Get("product"))
	roles := []map[string]string{}
	if key != "" {
		if product.IsKnown(key) {
			if p, ok := product.Resolve(key); ok && p.Class != "" {
				roles = append(roles, map[string]string{"key": p.Class, "label": product.ClassLabel(p.Class)})
			}
		} else {
			rs, _ := h.db.ProductRoles(r.Context(), key)
			for _, c := range rs {
				roles = append(roles, map[string]string{"key": c, "label": product.ClassLabel(c)})
			}
		}
	}
	appJSON(w, http.StatusOK, map[string]any{"product": key, "roles": roles})
}

// AppDeviceRole: POST /api/v1/app/device-role {"serial","product","class"} → {"ok","applied"}.
// An enrolled device takes it now; one not enrolled yet keeps it waiting for EnrollDevice
// (a day), which applies it when it is one of its model's roles.
func (h *Handler) AppDeviceRole(w http.ResponseWriter, r *http.Request, s *db.Session) {
	var body struct {
		Serial  string `json:"serial"`
		Product string `json:"product"`
		Class   string `json:"class"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		appErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	body.Serial, body.Class = strings.TrimSpace(body.Serial), strings.TrimSpace(body.Class)
	if body.Serial == "" || !product.IsClass(body.Class) {
		appErr(w, http.StatusBadRequest, "serial and a known class are required")
		return
	}
	if key := product.Normalize(body.Product); key != "" {
		if rs, _ := h.db.ProductRoles(r.Context(), key); !slices.Contains(rs, body.Class) {
			appErr(w, http.StatusBadRequest, "that role is not one of the model's roles")
			return
		}
	}
	d, err := h.db.GetDevice(r.Context(), body.Serial)
	if err == nil && d != nil && d.EnrollmentStatus == db.EnrollEnrolled {
		// An engineer only looks at a venue's devices (stateDecision), so a device already
		// placed at a venue keeps its role.
		if acc := h.accessFor(r.Context(), s.Role, s.Username); !acc.visible(d.ID) || acc.engineerVenueCount("notes", []uuid.UUID{d.ID}) > 0 {
			appErr(w, http.StatusForbidden, "that device is outside your access")
			return
		}
		if err := h.db.SetDeviceClass(r.Context(), body.Serial, body.Class, "enroll"); err != nil {
			if errors.Is(err, db.ErrRoleNotAllowed) {
				appErr(w, http.StatusBadRequest, "that role is not one of the model's roles")
				return
			}
			appErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		h.audit(r, "device.role", body.Serial, body.Class+" (enroll app, "+s.Username+")")
		h.hub.PublishDeviceUpdate(d.ID)
		appJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": true})
		return
	}
	if err := h.db.RequestDeviceRole(r.Context(), body.Serial, body.Class, s.Username); err != nil {
		appErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	appJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": false})
}
