package dashboard

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"mdm/internal/db"
	"mdm/internal/ratelimit"
	prod "mdm/internal/product"
)

// JSON endpoints for the desktop enroll app (tools/enroll-app). The app signs in with a
// normal dashboard account and gets the session id back as a bearer token, so people
// enrolling devices need no admin key and the app never carries one. The bearer is the
// same server-side session row the cookie points at: it expires, idles out, and is
// revoked by deleting the row (or POST /api/v1/app/logout).
//
// These routes are header-authenticated, not cookie-authenticated, so they are not
// reachable by a cross-site request and sit outside the same-origin guard.

const (
	appEnrollProfilePrefix = "AIO Enroll app · "
	appEnrollTokenDays     = 30
	appEnrollStatusMax     = 100
)

func appJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func appErr(w http.ResponseWriter, code int, msg string) {
	appJSON(w, code, map[string]string{"error": msg})
}

// appSession resolves "Authorization: Bearer <session id>" with the same expiry and
// idle rules as currentSession.
func (h *Handler) appSession(r *http.Request) (*db.Session, bool) {
	auth := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(auth) <= len(p) || !strings.EqualFold(auth[:len(p)], p) {
		return nil, false
	}
	sid := strings.TrimSpace(auth[len(p):])
	s, err := h.db.GetSession(r.Context(), sid)
	if err != nil {
		return nil, false
	}
	now := time.Now()
	if now.After(s.ExpiresAt) || now.Sub(s.LastSeen) > sessionIdleTimeout {
		_ = h.db.DeleteSession(r.Context(), sid)
		return nil, false
	}
	_ = h.db.TouchSession(r.Context(), sid)
	return s, true
}

// requireApp guards an app route: valid bearer session, and a role that may operate
// devices (the same ceiling the dashboard uses for enrolling).
func (h *Handler) requireApp(next func(http.ResponseWriter, *http.Request, *db.Session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.appSession(r)
		if !ok {
			appErr(w, http.StatusUnauthorized, "not signed in")
			return
		}
		if !roleCanOperate(s.Role) {
			appErr(w, http.StatusForbidden, "your role cannot enroll devices")
			return
		}
		next(w, r, s)
	}
}

// AppLogin: POST /api/v1/app/login {"username","password"} → {"token","username","role","expires_at"}.
// Shares the dashboard's failure limiter (8 per 15 min per IP and per account).
func (h *Handler) AppLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		appErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	ip := ratelimit.ClientIP(r)
	userKey := "u:" + body.Username
	for _, key := range []string{ip, userKey} {
		if n, retry := h.loginFails.Count(key); n >= loginMaxFailures {
			w.Header().Set("Retry-After", fmt.Sprint(int(retry.Seconds())+1))
			appErr(w, http.StatusTooManyRequests, fmt.Sprintf("too many failed attempts, try again in %d minute(s)", int(retry.Minutes())+1))
			return
		}
	}
	fail := func() {
		h.loginFails.Hit(ip)
		h.loginFails.Hit(userKey)
		appErr(w, http.StatusUnauthorized, "invalid credentials")
	}
	u, err := h.db.GetUserByUsername(r.Context(), body.Username)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(body.Password)) != nil {
		fail()
		return
	}
	if u.Email != nil && u.EmailVerifiedAt == nil {
		appErr(w, http.StatusForbidden, "verify your email before signing in")
		return
	}
	if !roleCanOperate(u.Role) {
		appErr(w, http.StatusForbidden, "your role cannot enroll devices")
		return
	}
	h.loginFails.Reset(ip)
	h.loginFails.Reset(userKey)

	sid, err := newSessionID()
	if err != nil {
		appErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	exp := time.Now().Add(time.Duration(h.cfg.SessionTimeout()) * time.Second)
	uid := u.ID
	if err := h.db.CreateSession(r.Context(), db.Session{
		ID: sid, UserID: &uid, Username: u.Username, Role: u.Role, ExpiresAt: exp,
	}); err != nil {
		appErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	appJSON(w, http.StatusOK, map[string]any{
		"token": sid, "username": u.Username, "role": u.Role, "expires_at": exp,
	})
}

// AppLogout: POST /api/v1/app/logout — kills the session row.
func (h *Handler) AppLogout(w http.ResponseWriter, r *http.Request, s *db.Session) {
	_ = h.db.DeleteSession(r.Context(), s.ID)
	appJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// AppEnrollStatus: GET /api/v1/app/enroll-status?serials=a,b,c →
// {"devices": {"<serial>": {"enrolled":bool,"status","class","agent_kind","agent_version","last_seen_at"}}}.
// A serial the server has never seen is reported enrolled=false.
func (h *Handler) AppEnrollStatus(w http.ResponseWriter, r *http.Request, _ *db.Session) {
	var serials []string
	seen := map[string]bool{}
	for _, s := range strings.Split(r.URL.Query().Get("serials"), ",") {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			serials = append(serials, s)
		}
	}
	if len(serials) == 0 {
		appErr(w, http.StatusBadRequest, "serials is required")
		return
	}
	if len(serials) > appEnrollStatusMax {
		appErr(w, http.StatusBadRequest, fmt.Sprintf("at most %d serials per call", appEnrollStatusMax))
		return
	}
	out := make(map[string]map[string]any, len(serials))
	for _, serial := range serials {
		d, err := h.db.GetDevice(r.Context(), serial)
		if err != nil || d == nil {
			out[serial] = map[string]any{"enrolled": false}
			continue
		}
		entry := map[string]any{
			"enrolled":     d.EnrollmentStatus == db.EnrollEnrolled,
			"status":       d.EnrollmentStatus,
			"class":        d.DeviceClass,
			"agent_kind":   d.AgentKind,
			"last_seen_at": d.LastSeenAt,
		}
		var extra struct {
			AgentVersion string `json:"agent_version"`
		}
		if len(d.LatestExtra) > 0 && json.Unmarshal(d.LatestExtra, &extra) == nil && extra.AgentVersion != "" {
			entry["agent_version"] = extra.AgentVersion
		}
		out[serial] = entry
	}
	appJSON(w, http.StatusOK, map[string]any{"devices": out})
}

// AppEnrollToken: POST /api/v1/app/enroll-token {"device_class"} → {"token","server_url","device_class","expires_at"}.
// Hands back the live token of this app's profile for the class, minting one (30 days)
// when none is active. One shared profile per class keeps the Enrollment page tidy.
func (h *Handler) AppEnrollToken(w http.ResponseWriter, r *http.Request, s *db.Session) {
	var body struct {
		DeviceClass string `json:"device_class"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		appErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	class := strings.ToLower(strings.TrimSpace(body.DeviceClass))
	if class == "" || !prod.IsClass(class) {
		appErr(w, http.StatusBadRequest, "unknown device_class")
		return
	}
	name := appEnrollProfilePrefix + class

	profiles, err := h.db.ListEnrollmentProfiles(r.Context())
	if err != nil {
		appErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	for _, p := range profiles {
		// Skip a token about to lapse so a long enroll never fails half way.
		if p.Name == name && p.Active() && (p.ExpiresAt == nil || time.Until(*p.ExpiresAt) > 24*time.Hour) {
			appJSON(w, http.StatusOK, appTokenResp(h.baseURL(r), p))
			return
		}
	}

	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		appErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	exp := time.Now().Add(appEnrollTokenDays * 24 * time.Hour)
	id, err := h.db.CreateEnrollmentProfile(r.Context(), db.EnrollmentProfileInput{
		Name: name, Token: "enr_" + hex.EncodeToString(raw), DeviceClass: class, ExpiresAt: &exp,
		Notes: "Minted by the AIO Enroll desktop app, first used by " + s.Username,
	})
	if err != nil {
		appErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	p, err := h.db.GetEnrollmentProfile(r.Context(), id)
	if err != nil || p == nil {
		appErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.audit(r, "enrollment_profile.create", id.String(), name+" (enroll app, "+s.Username+")")
	appJSON(w, http.StatusCreated, appTokenResp(h.baseURL(r), *p))
}

func appTokenResp(serverURL string, p db.EnrollmentProfile) map[string]any {
	return map[string]any{
		"token": p.Token, "server_url": serverURL,
		"device_class": p.DeviceClass, "expires_at": p.ExpiresAt,
	}
}

// appUser loads the account behind a session. The static env admin has no users row.
func (h *Handler) appUser(r *http.Request, s *db.Session) *db.User {
	if s.UserID == nil {
		return nil
	}
	u, err := h.db.GetUser(r.Context(), *s.UserID)
	if err != nil {
		return nil
	}
	return u
}

// AppMe: GET /api/v1/app/me → {"username","role","name","has_avatar","avatar_ver"}.
func (h *Handler) AppMe(w http.ResponseWriter, r *http.Request, s *db.Session) {
	out := map[string]any{"username": s.Username, "role": s.Role, "name": s.Username, "has_avatar": false}
	if u := h.appUser(r, s); u != nil {
		if n := strings.TrimSpace(u.FirstName + " " + u.LastName); n != "" {
			out["name"] = n
		}
		out["has_avatar"] = u.HasAvatar()
		out["avatar_ver"] = u.AvatarVer
	}
	appJSON(w, http.StatusOK, out)
}

// AppAvatar: GET /api/v1/app/avatar → the signed-in user's profile picture (PNG), 404 if none.
func (h *Handler) AppAvatar(w http.ResponseWriter, r *http.Request, s *db.Session) {
	if s.UserID == nil {
		http.NotFound(w, r)
		return
	}
	png, ver, err := h.db.GetUserAvatar(r.Context(), uuid.UUID(*s.UserID))
	if err != nil || len(png) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("ETag", fmt.Sprintf(`"av-%d"`, ver))
	_, _ = w.Write(png)
}

func (h *Handler) registerAppRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/app/login", h.AppLogin)
	mux.HandleFunc("POST /api/v1/app/logout", h.requireApp(h.AppLogout))
	mux.HandleFunc("GET /api/v1/app/me", h.requireApp(h.AppMe))
	mux.HandleFunc("GET /api/v1/app/avatar", h.requireApp(h.AppAvatar))
	mux.HandleFunc("GET /api/v1/app/enroll-status", h.requireApp(h.AppEnrollStatus))
	mux.HandleFunc("POST /api/v1/app/classify", h.requireApp(h.AppClassify))
	mux.HandleFunc("POST /api/v1/app/enroll-token", h.requireApp(h.AppEnrollToken))
}
