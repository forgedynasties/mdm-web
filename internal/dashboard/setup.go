package dashboard

import (
	"context"
	"encoding/json"
	_ "image/gif"
	_ "image/jpeg"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// AppLibraryPage renders /apps: the library with upload, for admin, dev and super op.
func (h *Handler) AppLibraryPage(w http.ResponseWriter, r *http.Request) {
	families, suggestions, mode := h.libraryData(r.Context())
	apps, _ := h.db.ListApps(r.Context())
	cov := h.appCoverage(r.Context(), apps)
	roleApps, _ := h.db.ListRoleApps(r.Context())
	installs, _ := h.db.AppInstallsDaily(r.Context(), 14)
	added, _ := h.db.AppsAddedSince(r.Context(), time.Now().AddDate(0, 0, -30))
	h.render(w, r, "apps.html", map[string]any{
		"AppsTab": "library",
		// Only the role apps (Lineup) unless "Show all apps" is ticked (user, 26 Oct 2026).
		"ShowAllApps": r.URL.Query().Get("all") == "1",
		// A1 (picked 21 Oct 2026): banner, cards, one app table and a rail. apps_a1.go.
		"A1": buildAppsA1(a1Inputs{Families: families, Apps: apps, Cov: cov, RoleApps: roleApps,
			Installs: installs, AddedLast: added, Now: time.Now()}),
		"Title":         "App library",
		"ActivePage":    "apps",
		"Apps":          apps,
		"Families":      families,
		"Suggestions":   suggestions,
		"AppFamilyMode": mode,
		// What is actually installed out there, against what the library holds. The page
		// listed what COULD be installed and said nothing about the fleet, so nobody could
		// ask "is this app out there, and is it the version we think". See app_coverage.go.
		"Coverage": cov,
		// Shelves: what needs attention, what the fleet runs, what was uploaded and never
		// rolled out, what arrived recently. See app_store.go.
		"Store": h.appStore(r.Context(), families, cov, apps),
	})
}

// ── Setup ─────────────────────────────────────────────────────────────────────

func (h *Handler) SetupPage(w http.ResponseWriter, r *http.Request) {
	apps, err := h.db.ListApps(r.Context())
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	h.render(w, r, "setup.html", map[string]any{
		"Title": "App Repository",
		"Apps":  apps,
	})
}

func (h *Handler) SetupCreateApp(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	apkURL := strings.TrimSpace(r.FormValue("apk_url"))
	pkg := strings.TrimSpace(r.FormValue("package_name"))
	if name == "" || apkURL == "" {
		http.Redirect(w, r, "/apps", http.StatusFound)
		return
	}
	if _, err := h.db.CreateApp(r.Context(), name, apkURL, pkg); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/apps", http.StatusFound)
}

func (h *Handler) SetupCreateAppJSON(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	apkURL := strings.TrimSpace(r.FormValue("apk_url"))
	pkg := strings.TrimSpace(r.FormValue("package_name"))
	if name == "" || apkURL == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	app, err := h.db.CreateApp(r.Context(), name, apkURL, pkg)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(app)
}

// ---- S3 APK uploads: browser uploads directly to S3 via a presigned PUT ------

// AppUploadURL returns a presigned S3 PUT URL so the browser sends the APK straight
// to S3, never through this server. Admin only.
func (h *Handler) AppUploadURL(w http.ResponseWriter, r *http.Request) {
	if h.apk == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "APK upload is not configured")
		return
	}
	key := h.apk.Key(uuid.NewString() + ".apk")
	url, err := h.apk.PresignPut(r.Context(), key, "application/vnd.android.package-archive", 15*time.Minute)
	if err != nil {
		log.Printf("presign put: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "could not create upload URL")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"url": url, "key": key})
}

// AppRegister reads the freshly-uploaded object back from S3, parses its package,
// version and launcher icon, creates the repository app, and feeds the shared icon
// index (so the icon shows everywhere immediately). Admin only.
func (h *Handler) AppRegister(w http.ResponseWriter, r *http.Request) {
	if h.apk == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "APK upload is not configured")
		return
	}
	var req struct {
		Key string `json:"key"`
		// Client-parsed metadata (fast path — the browser already has the file, so it
		// parses locally and the server never downloads the APK from S3). If Package is
		// empty, the server falls back to parsing the object itself.
		Package string `json:"package"`
		Version string `json:"version"`
		Name    string `json:"name"`
		Icon    string `json:"icon"` // base64 PNG
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if req.Key == "" {
		writeJSONError(w, http.StatusBadRequest, "key required")
		return
	}
	pkg, version, name, icon := req.Package, req.Version, req.Name, req.Icon
	if pkg == "" {
		// Fallback: no client metadata — parse the object server-side (slower).
		meta, err := h.apk.Parse(r.Context(), req.Key)
		if err != nil {
			log.Printf("apk parse %s: %v", req.Key, err)
			writeJSONError(w, http.StatusBadRequest, "could not parse APK — is it a valid .apk?")
			return
		}
		pkg, version, name, icon = meta.Package, meta.VersionName, meta.Label, meta.IconPNGB64
	}
	if len(icon) > 128*1024 { // safety cap; a 96px PNG is far smaller
		icon = ""
	}
	if name == "" {
		name = pkg
	}
	// apk_url must be an absolute, device-reachable URL (the device fetches it directly).
	base := h.baseURL(r)
	app, err := h.db.CreateS3App(r.Context(), name, pkg, version, req.Key, base)
	if err != nil {
		log.Printf("create s3 app: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "could not save app")
		return
	}
	if icon != "" {
		if err := h.db.UpsertAppIcon(r.Context(), pkg, icon); err != nil {
			log.Printf("upsert app icon: %v", err)
		}
	}
	// Warm the local cache in the background so device installs serve from LAN disk
	// (S3 here is too slow to stream a large APK within the device's download timeout).
	go func(key string, id uuid.UUID) {
		// The signer too, so a later pull of this package from a device can be checked
		// against it (apk_pull.go).
		if s, err := h.apk.SignerSHA256(context.Background(), key); err == nil {
			_ = h.db.SetAppSigner(context.Background(), id, s)
		}
		if err := h.apk.EnsureCached(context.Background(), key); err != nil {
			log.Printf("apk cache warm %s: %v", key, err)
		}
	}(req.Key, app.ID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(app)
}

// apkDirectS3 sends devices straight to S3 for library APKs (scalability plan W4, 29 Oct
// 2026): a 302 to a presigned GET instead of copying the bytes through this process, so a
// rollout costs the server no bandwidth and a server restart cuts no download. Off unless
// APK_DIRECT_S3=1, until a real install has been timed both ways on stage: the disk cache
// below exists because streaming from S3 once ran at ~0.2 MB/s. Both clients follow the
// redirect; the firmware client's Range/If-Range resume keeps working because the install
// payload's ETag is read by a HEAD on this URL, which follows the redirect to S3's own.
var apkDirectS3 = os.Getenv("APK_DIRECT_S3") == "1"

// apkPresignTTL outlives the firmware client's 15-minute download budget with room to spare.
const apkPresignTTL = 6 * time.Hour

// AppAPK serves a library APK: from the local disk cache (Range-aware), else streamed
// from S3, or with APK_DIRECT_S3 a 302 to a presigned S3 URL. Open: the device's package
// installer fetches it by plain URL with no dashboard session.
func (h *Handler) AppAPK(w http.ResponseWriter, r *http.Request) {
	if h.apk == nil {
		http.Error(w, "not configured", http.StatusServiceUnavailable)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	app, err := h.db.GetApp(r.Context(), id)
	if err != nil || app.S3Key == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if apkDirectS3 {
		if u, err := h.apk.PresignGet(r.Context(), app.S3Key, apkPresignTTL); err == nil {
			w.Header().Set("Cache-Control", "no-store") // a presigned URL is per request
			http.Redirect(w, r, u, http.StatusFound)
			return
		} else {
			log.Printf("apk presign %s: %v (serving it here)", app.S3Key, err)
		}
	}
	w.Header().Set("Content-Type", "application/vnd.android.package-archive")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+app.PackageName+".apk\"")
	// Prefer the local cache: serving from disk is LAN-fast and http.ServeFile supports
	// Range/resume, so a device that lost its connection can continue where it left off.
	// (S3 here is ~0.2 MB/s, far too slow to stream a large APK within the device's
	// download timeout — that caused the endless progress reset.)
	if path, ok := h.apk.CachedPath(app.S3Key); ok {
		http.ServeFile(w, r, path)
		return
	}
	// Not cached yet: kick off a background fill so future installs are fast, and stream
	// from S3 this time as a fallback — honoring the client's Range header so a dropped
	// slow transfer RESUMES instead of restarting (the device sends Range on retry; if we
	// ignored it and returned 200, the client would delete its partial and start over).
	go func(key string) {
		if err := h.apk.EnsureCached(context.Background(), key); err != nil {
			log.Printf("apk cache %s: %v", key, err)
		}
	}(app.S3Key)
	body, size, ct, contentRange, err := h.apk.Get(r.Context(), app.S3Key, r.Header.Get("Range"))
	if err != nil {
		log.Printf("apk get %s: %v", app.S3Key, err)
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Accept-Ranges", "bytes")
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	if contentRange != "" { // S3 returned a partial → mirror it as 206
		w.Header().Set("Content-Range", contentRange)
		w.WriteHeader(http.StatusPartialContent)
	}
	io.Copy(w, body)
}

// SetupUpdateApp edits an existing repository app (name, APK URL, package name).
func (h *Handler) SetupUpdateApp(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid app ID", http.StatusBadRequest)
		return
	}
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	apkURL := strings.TrimSpace(r.FormValue("apk_url"))
	pkg := strings.TrimSpace(r.FormValue("package_name"))
	if name == "" || apkURL == "" {
		http.Redirect(w, r, "/apps", http.StatusFound)
		return
	}
	if _, err := h.db.UpdateApp(r.Context(), id, name, apkURL, pkg); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/apps", http.StatusFound)
}

func (h *Handler) SetupDeleteApp(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid app ID", http.StatusBadRequest)
		return
	}
	if err := h.db.DeleteApp(r.Context(), id); err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	_ = h.db.DeleteEmptyAppFamilies(r.Context())
	h.audit(r, "apps.delete", id.String(), "")
	if r.Header.Get("X-Requested-With") == "fetch" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Back to the library the user was on (/apps or Settings), never the old /setup.
	if ref := r.Header.Get("Referer"); strings.Contains(ref, "/settings") {
		http.Redirect(w, r, "/apps", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/apps", http.StatusSeeOther)
}
