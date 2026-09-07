package dashboard

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strings"

	"github.com/google/uuid"
	qrcode "github.com/skip2/go-qrcode"
)

// This file holds the enrollment feature pages. Kept separate from the huge
// handlers.go so the feature surface is easy to find and extend. All pages render
// through h.render (which injects role/brand/nav via withRole).

// EnrollmentPage shows enrollment profiles (revocable QR/zero-touch tokens) plus the
// manual adb provisioning path with the shared device API key.
func (h *Handler) EnrollmentPage(w http.ResponseWriter, r *http.Request) {
	deviceKey := os.Getenv("DEVICE_API_KEY")
	masked := deviceKey
	if len(masked) > 6 {
		masked = masked[:3] + "••••••" + masked[len(masked)-3:]
	}
	profiles, _ := h.db.ListEnrollmentProfiles(r.Context())
	groups, _ := h.db.ListGroups(r.Context())
	h.render(w, r, "enrollment.html", map[string]any{
		"Title":          "Enrollment",
		"ActivePage":     "enrollment",
		"ServerURL":      h.baseURL(r),
		"DeviceKey":      deviceKey,
		"DeviceKeyMask":  masked,
		"AdminComponent": "com.skorra.agent/com.skorra.agent.MdmDeviceAdminReceiver",
		"AgentPackage":   "com.skorra.agent",
		"Profiles":       profiles,
		"Groups":         groups,
		"HasAgentAPK":    os.Getenv("AGENT_APK_URL") != "" && os.Getenv("AGENT_APK_CHECKSUM") != "",
	})
}

// EnrollmentProfileCreate makes a new enrollment profile with a fresh "enr_..." token.
func (h *Handler) EnrollmentProfileCreate(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		h.hxDoneToast(w, r, "/enrollment", "Profile name is required", "error")
		return
	}
	var groupID *uuid.UUID
	if gid := r.FormValue("group_id"); gid != "" {
		if parsed, err := uuid.Parse(gid); err == nil {
			groupID = &parsed
		}
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	token := "enr_" + hex.EncodeToString(raw)
	if _, err := h.db.CreateEnrollmentProfile(r.Context(), name, token, groupID, strings.TrimSpace(r.FormValue("notes"))); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.hxDoneToast(w, r, "/enrollment", "Enrollment profile created", "success")
}

func (h *Handler) enrollmentProfileSetRevoked(w http.ResponseWriter, r *http.Request, revoked bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Bad id", http.StatusBadRequest)
		return
	}
	if err := h.db.SetEnrollmentProfileRevoked(r.Context(), id, revoked); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	msg := "Profile reactivated"
	if revoked {
		msg = "Profile revoked — its QR codes stop working immediately"
	}
	h.hxDoneToast(w, r, "/enrollment", msg, "success")
}

func (h *Handler) EnrollmentProfileRevoke(w http.ResponseWriter, r *http.Request) {
	h.enrollmentProfileSetRevoked(w, r, true)
}

func (h *Handler) EnrollmentProfileActivate(w http.ResponseWriter, r *http.Request) {
	h.enrollmentProfileSetRevoked(w, r, false)
}

func (h *Handler) EnrollmentProfileDelete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Bad id", http.StatusBadRequest)
		return
	}
	if err := h.db.DeleteEnrollmentProfile(r.Context(), id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.hxDoneToast(w, r, "/enrollment", "Profile deleted", "success")
}

// EnrollmentProfileQR renders the Android managed-provisioning QR payload for a profile
// as a PNG. Scanned from the setup wizard (tap the welcome screen 6×), it installs the
// agent as Device Owner and hands it the server URL + enrollment token.
func (h *Handler) EnrollmentProfileQR(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Bad id", http.StatusBadRequest)
		return
	}
	p, err := h.db.GetEnrollmentProfile(r.Context(), id)
	if err != nil {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	payload := map[string]any{
		"android.app.extra.PROVISIONING_DEVICE_ADMIN_COMPONENT_NAME":   "com.skorra.agent/com.skorra.agent.MdmDeviceAdminReceiver",
		"android.app.extra.PROVISIONING_LEAVE_ALL_SYSTEM_APPS_ENABLED": true,
		"android.app.extra.PROVISIONING_ADMIN_EXTRAS_BUNDLE": map[string]string{
			"server_url":   h.baseURL(r),
			"enroll_token": p.Token,
		},
	}
	// QR provisioning needs a downloadable agent APK; without these env vars the code
	// still carries the extras (usable for docs/manual flows) but can't cold-provision.
	if apkURL := os.Getenv("AGENT_APK_URL"); apkURL != "" {
		payload["android.app.extra.PROVISIONING_DEVICE_ADMIN_PACKAGE_DOWNLOAD_LOCATION"] = apkURL
	}
	if sum := os.Getenv("AGENT_APK_CHECKSUM"); sum != "" {
		payload["android.app.extra.PROVISIONING_DEVICE_ADMIN_SIGNATURE_CHECKSUM"] = sum
	}
	blob, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	png, err := qrcode.Encode(string(blob), qrcode.Medium, 512)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store") // carries a live credential
	w.Write(png)
}
