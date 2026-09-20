package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
	prod "mdm/internal/product"
)

// Enrollment profiles over the admin API (X-API-Key), so a build machine can mint the
// token it needs to bake into an app — the MDM-lite host apps carry their server URL
// and token at compile time — instead of someone copying one out of the dashboard.
// Same rules as the dashboard form: it creates the token, the profile carries the
// intent (class, site, group) applied to every device that enrolls with it.
//
// The token is a live credential: anyone holding it can enroll a device. That is the
// same trust level the rest of this API already assumes (see the security note in
// docs/API_REFERENCE.md — the admin key is fleet-wide RCE via shell), so profiles are
// listed with their tokens rather than pretending otherwise.

// enrollmentProfileJSON is one profile as the API reports it.
type enrollmentProfileJSON struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Token        string     `json:"token"`
	Notes        string     `json:"notes,omitempty"`
	DeviceClass  string     `json:"device_class,omitempty"`
	GroupID      *uuid.UUID `json:"group_id,omitempty"`
	GroupName    string     `json:"group,omitempty"`
	RestaurantID *uuid.UUID `json:"restaurant_id,omitempty"`
	Restaurant   string     `json:"site,omitempty"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	MaxEnrolls   *int       `json:"max_enrolls,omitempty"`
	EnrollCount  int        `json:"enroll_count"`
	Active       bool       `json:"active"`
	CreatedAt    time.Time  `json:"created_at"`
}

func enrollmentProfileToJSON(p db.EnrollmentProfile) enrollmentProfileJSON {
	return enrollmentProfileJSON{
		ID: p.ID.String(), Name: p.Name, Token: p.Token, Notes: p.Notes,
		DeviceClass: p.DeviceClass, GroupID: p.GroupID, GroupName: p.GroupName,
		RestaurantID: p.RestaurantID, Restaurant: p.RestaurantName,
		ExpiresAt: p.ExpiresAt, MaxEnrolls: p.MaxEnrolls,
		EnrollCount: p.EnrollCount, Active: p.Active(), CreatedAt: p.CreatedAt,
	}
}

// ListEnrollmentProfiles returns every profile, newest first, tokens included.
func (h *Handler) ListEnrollmentProfiles(w http.ResponseWriter, r *http.Request) {
	profiles, err := h.db.ListEnrollmentProfiles(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	out := make([]enrollmentProfileJSON, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, enrollmentProfileToJSON(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"profiles": out})
}

// CreateEnrollmentProfile mints a profile and its token. Only name is required; the
// rest is the intent devices inherit when they enroll through it.
func (h *Handler) CreateEnrollmentProfile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name         string `json:"name"`
		Notes        string `json:"notes"`
		DeviceClass  string `json:"device_class"`
		GroupID      string `json:"group_id"`
		RestaurantID string `json:"restaurant_id"`
		ExpiresDays  int    `json:"expires_days"`
		MaxEnrolls   int    `json:"max_enrolls"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	in := db.EnrollmentProfileInput{Name: name, Notes: strings.TrimSpace(body.Notes)}
	if c := strings.ToLower(strings.TrimSpace(body.DeviceClass)); c != "" {
		if !prod.IsClass(c) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown device_class " + c})
			return
		}
		in.DeviceClass = c
	}
	if s := strings.TrimSpace(body.GroupID); s != "" {
		id, err := uuid.Parse(s)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid group_id"})
			return
		}
		in.GroupID = &id
	}
	if s := strings.TrimSpace(body.RestaurantID); s != "" {
		id, err := uuid.Parse(s)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid restaurant_id"})
			return
		}
		in.RestaurantID = &id
	}
	if body.ExpiresDays > 0 {
		t := time.Now().Add(time.Duration(body.ExpiresDays) * 24 * time.Hour)
		in.ExpiresAt = &t
	}
	if body.MaxEnrolls > 0 {
		n := body.MaxEnrolls
		in.MaxEnrolls = &n
	}
	// The token is minted here, never accepted from the caller: a token someone chose
	// is a token someone can guess.
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	in.Token = "enr_" + hex.EncodeToString(raw)

	id, err := h.db.CreateEnrollmentProfile(r.Context(), in)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	p, err := h.db.GetEnrollmentProfile(r.Context(), id)
	if err != nil || p == nil {
		// Created, but we cannot read it back: still hand over the token, which is the
		// only part the caller cannot recover on its own.
		writeJSON(w, http.StatusCreated, map[string]any{"id": id.String(), "token": in.Token})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"profile": enrollmentProfileToJSON(*p)})
}
