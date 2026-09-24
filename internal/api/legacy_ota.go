package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"mdm/internal/db"
	"mdm/internal/ota"
	"mdm/internal/ratelimit"
	"mdm/internal/safehttp"
)

// Legacy OTA: the wire protocol of com.aioapp.otautil, the system app on builds
// that predate the MDM agent. Those builds carry the server address (this host,
// port 8000) in the firmware, so the MDM answers that port itself:
//
//	POST /device/poll          {serial_number, build_id}            -> {"status":"ok"}
//	POST /device/check-update  {serial_number, build_id}            -> {update_available, update_url, payload_offset, payload_size, payload_headers}
//	POST /device/update-status {serial_number, build_id, phase, progress?, error?}
//	GET  /device/pkg/{token}   the package bytes (Range aware), counted for progress
//
// These devices never enter the fleet tables. They live in legacy_ota_devices and
// are steered by legacy_ota_groups (allowlist of serials + one target release),
// the same model the old ota-server had, shown under Updates › Legacy OTA.
//
// A Settings toggle picks who answers: the MDM ("mdm") or the old ota-server
// container, forwarded verbatim ("passthrough"). The app polls every 15 minutes
// at least and never reports status on its own, so download progress comes from
// the bytes streamed through /device/pkg, and completion from the next poll on
// the new build.

// legacyOTA holds the pieces that are not on the main device API.
type legacyOTA struct {
	upstream  *url.URL
	proxy     *httputil.ReverseProxy
	secret    []byte
	dl        *http.Client
	metaMu    sync.Mutex
	metaCache map[int]*ota.Payload
}

// SetLegacyOTAUpstream configures the pass-through target (the old ota-server).
func (h *Handler) SetLegacyOTAUpstream(raw string) {
	h.legacyInit()
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		log.Printf("[legacy-ota] bad upstream %q, pass-through disabled", raw)
		return
	}
	h.legacy.upstream = u
	h.legacy.proxy = httputil.NewSingleHostReverseProxy(u)
	h.legacy.proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("[legacy-ota] pass-through to %s failed: %v", u.Host, err)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}
}

func (h *Handler) legacyInit() {
	if h.legacy == nil {
		h.legacy = &legacyOTA{
			secret:    []byte(h.adminAPIKey + "|legacy-ota"),
			dl:        safehttp.Client(0), // multi-GB streams: no overall timeout, dial timeout still applies
			metaCache: map[int]*ota.Payload{},
		}
	}
}

// LegacyOTAMux is the handler for the legacy OTA listener. Only the otautil
// routes exist here; everything else is a 404 so the dashboard is not reachable
// on this port.
func (h *Handler) LegacyOTAMux() http.Handler {
	h.legacyInit()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /device/poll", h.legacyGate(h.LegacyPoll))
	mux.HandleFunc("POST /device/check-update", h.legacyGate(h.LegacyCheckUpdate))
	mux.HandleFunc("POST /device/update-status", h.legacyGate(h.LegacyUpdateStatus))
	mux.HandleFunc("GET /device/pkg/{token}", h.LegacyPackage)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "mode": h.cfg.LegacyOTAMode()})
	})
	return mux
}

// legacyGate forwards to the old server in pass-through mode, else serves locally.
func (h *Handler) legacyGate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.LegacyOTAMode() == "passthrough" {
			if h.legacy.proxy == nil {
				http.Error(w, "pass-through target not configured", http.StatusBadGateway)
				return
			}
			h.legacy.proxy.ServeHTTP(w, r)
			return
		}
		next(w, r)
	}
}

type legacyDeviceReq struct {
	SerialNumber string `json:"serial_number"`
	BuildID      string `json:"build_id"`
	Phase        string `json:"phase,omitempty"`
	Progress     *int   `json:"progress,omitempty"`
	Error        string `json:"error,omitempty"`
}

// legacyIdentify validates the body, throttles, and records the contact in the
// legacy table (never in devices).
func (h *Handler) legacyIdentify(w http.ResponseWriter, r *http.Request, isCheck bool) (*legacyDeviceReq, bool) {
	var req legacyDeviceReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 8*1024)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return nil, false
	}
	req.SerialNumber = strings.TrimSpace(req.SerialNumber)
	req.BuildID = strings.TrimSpace(req.BuildID)
	if req.SerialNumber == "" || req.BuildID == "" || len(req.SerialNumber) > maxSerialLen || len(req.BuildID) > maxBuildIDLen {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "serial_number and build_id are required"})
		return nil, false
	}
	if !validSerial(req.SerialNumber) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errInvalidSerial})
		return nil, false
	}
	if h.deviceRateLimited(w, req.SerialNumber) {
		return nil, false
	}
	if err := h.db.TouchLegacyOTADevice(r.Context(), req.SerialNumber, req.BuildID, ratelimit.ClientIP(r), isCheck); err != nil {
		log.Printf("[legacy-ota] record %s: %v", req.SerialNumber, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return nil, false
	}
	// Reporting the build a deployment offered closes that row, and the rollout
	// once every device has settled.
	_ = h.db.CompleteLegacyDeploymentsAtBuild(r.Context(), req.SerialNumber, req.BuildID)
	// A poll from a device that is mid-install is a chance to (re)attach the
	// update_engine watcher: the download may have finished while its MDM socket was
	// down, leaving the row with no progress source at all.
	if dev, err := h.db.GetLegacyOTADevice(r.Context(), req.SerialNumber); err == nil && dev != nil {
		switch dev.Status {
		case "installing", "verifying", "finalizing":
			h.watchLegacyInstall(req.SerialNumber, h.legacyDeploymentFor(r.Context(), req.SerialNumber))
		}
	}
	return &req, true
}

// LegacyPoll is the app's heartbeat.
func (h *Handler) LegacyPoll(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.legacyIdentify(w, r, false); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type legacyUpdateResp struct {
	UpdateAvailable bool     `json:"update_available"`
	UpdateURL       string   `json:"update_url,omitempty"`
	PayloadOffset   *int64   `json:"payload_offset,omitempty"`
	PayloadSize     *int64   `json:"payload_size,omitempty"`
	PayloadHeaders  []string `json:"payload_headers,omitempty"`
}

// LegacyCheckUpdate answers from the device's group: enabled, with a target
// release that has an applicable package, and the device not yet on that build.
func (h *Handler) LegacyCheckUpdate(w http.ResponseWriter, r *http.Request) {
	req, ok := h.legacyIdentify(w, r, true)
	if !ok {
		return
	}
	none := func() { writeJSON(w, http.StatusOK, legacyUpdateResp{UpdateAvailable: false}) }
	dep, target, err := h.db.ResolveLegacyDeployment(r.Context(), req.SerialNumber)
	if err != nil {
		log.Printf("[legacy-ota] resolve deployment for %s: %v", req.SerialNumber, err)
		none()
		return
	}
	if dep == nil || dep.ReleaseVersion == "" || dep.ReleaseVersion == req.BuildID {
		none()
		return
	}
	pkg, err := h.db.PickLegacyOTAPackage(r.Context(), dep.ReleaseID, req.BuildID)
	if err != nil || pkg == nil {
		if err != nil {
			log.Printf("[legacy-ota] package for %s (release %d): %v", req.SerialNumber, dep.ReleaseID, err)
		}
		none()
		return
	}
	_ = target
	meta, err := h.legacyPayloadMeta(r.Context(), pkg)
	if err != nil {
		log.Printf("[legacy-ota] payload metadata for package %d (%s): %v", pkg.ID, pkg.UpdateURL, err)
		none()
		return
	}
	_ = h.db.SetLegacyOTADeviceStatus(r.Context(), req.SerialNumber, "offered", pkg.TargetBuildID, "")
	_ = h.db.SetLegacyDeploymentDevice(r.Context(), dep.ID, req.SerialNumber, "offered", 0, "", pkg.TargetBuildID)
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	dlURL := scheme + "://" + r.Host + "/device/pkg/" + h.legacyToken(pkg.ID, req.SerialNumber)
	log.Printf("[legacy-ota] %s on %s -> %s (deployment %d, package %d %s)", req.SerialNumber, req.BuildID, pkg.TargetBuildID, dep.ID, pkg.ID, pkg.Type)
	off, size := meta.Offset, meta.Size
	writeJSON(w, http.StatusOK, legacyUpdateResp{UpdateAvailable: true, UpdateURL: dlURL, PayloadOffset: &off, PayloadSize: &size, PayloadHeaders: meta.Headers})
}

// legacyPayloadMeta returns the payload location for a package, computed once from
// the zip's central directory and cached in the row.
func (h *Handler) legacyPayloadMeta(ctx context.Context, pkg *db.OTAPackage) (*ota.Payload, error) {
	h.legacy.metaMu.Lock()
	if m := h.legacy.metaCache[pkg.ID]; m != nil {
		h.legacy.metaMu.Unlock()
		return m, nil
	}
	h.legacy.metaMu.Unlock()
	if off, size, headers, ok, err := h.db.GetOTAPayloadMeta(ctx, pkg.ID); err == nil && ok {
		m := &ota.Payload{Offset: off, Size: size, Headers: headers}
		h.legacy.metaMu.Lock()
		h.legacy.metaCache[pkg.ID] = m
		h.legacy.metaMu.Unlock()
		return m, nil
	}
	m, err := ota.PayloadInfo(ctx, pkg.UpdateURL)
	if err != nil {
		return nil, err
	}
	if err := h.db.SetOTAPayloadMeta(ctx, pkg.ID, m.Offset, m.Size, m.Headers); err != nil {
		log.Printf("[legacy-ota] cache payload meta for package %d: %v", pkg.ID, err)
	}
	h.legacy.metaMu.Lock()
	h.legacy.metaCache[pkg.ID] = m
	h.legacy.metaMu.Unlock()
	return m, nil
}

// legacyToken binds a download to one package and one serial: pkgID.serial.mac.
func (h *Handler) legacyToken(pkgID int, serial string) string {
	body := strconv.Itoa(pkgID) + "." + base64.RawURLEncoding.EncodeToString([]byte(serial))
	mac := hmac.New(sha256.New, h.legacy.secret)
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
}

func (h *Handler) legacyParseToken(tok string) (int, string, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return 0, "", false
	}
	pkgID, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, "", false
	}
	serial := string(raw)
	if !hmac.Equal([]byte(h.legacyToken(pkgID, serial)), []byte(tok)) {
		return 0, "", false
	}
	return pkgID, serial, true
}

// LegacyPackage streams the package from its stored URL to the device, honouring
// Range (the app resumes), and turns the bytes sent into download progress.
func (h *Handler) LegacyPackage(w http.ResponseWriter, r *http.Request) {
	pkgID, serial, ok := h.legacyParseToken(r.PathValue("token"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	pkg, err := h.db.GetOTAPackage(r.Context(), pkgID)
	if err != nil || pkg == nil || pkg.Status != "active" {
		http.NotFound(w, r)
		return
	}
	src := pkg.UpdateURL
	if err := safehttp.CheckURL(src, false); err != nil {
		http.Error(w, "package URL not allowed", http.StatusBadGateway)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, src, nil)
	if err != nil {
		http.Error(w, "bad package URL", http.StatusBadGateway)
		return
	}
	var start int64
	if rg := r.Header.Get("Range"); rg != "" {
		req.Header.Set("Range", rg)
		if strings.HasPrefix(rg, "bytes=") {
			if n, err := strconv.ParseInt(strings.SplitN(strings.TrimPrefix(rg, "bytes="), "-", 2)[0], 10, 64); err == nil {
				start = n
			}
		}
	}
	resp, err := h.legacy.dl.Do(req)
	if err != nil {
		log.Printf("[legacy-ota] fetch %s: %v", src, err)
		http.Error(w, "package unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		http.Error(w, "package unavailable", http.StatusBadGateway)
		return
	}
	for _, k := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	total := resp.ContentLength
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		if i := strings.LastIndexByte(cr, '/'); i >= 0 {
			if n, err := strconv.ParseInt(cr[i+1:], 10, 64); err == nil {
				total = n
			}
		}
	}
	_ = h.db.SetLegacyOTADeviceStatus(r.Context(), serial, "downloading", "", "")
	depID := h.legacyDeploymentFor(r.Context(), serial)
	if depID > 0 {
		_ = h.db.SetLegacyDeploymentDevice(r.Context(), depID, serial, "downloading", 0, "", "")
	}
	w.WriteHeader(resp.StatusCode)
	buf := make([]byte, 256*1024)
	sent := start
	lastPct := -1
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // device went away; it resumes with a Range later
			}
			sent += int64(n)
			if total > 0 {
				if pct := int(sent * 100 / total); pct != lastPct {
					lastPct = pct
					ota.Legacy.Set(serial, "downloading", pct)
					if depID > 0 && pct%5 == 0 {
						_ = h.db.SetLegacyDeploymentDevice(context.Background(), depID, serial, "downloading", pct, "", "")
					}
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return
		}
	}
	if total > 0 && sent >= total {
		// The app moves the file and hands it to update_engine right away; without a
		// status report this stays "installing" until the next poll on the new build.
		ota.Legacy.Set(serial, "installing", 0)
		_ = h.db.SetLegacyOTADeviceStatus(context.Background(), serial, "installing", "", "")
		if depID > 0 {
			_ = h.db.SetLegacyDeploymentDevice(context.Background(), depID, serial, "installing", 0, "", "")
		}
		log.Printf("[legacy-ota] package %d fully sent to %s", pkg.ID, serial)
		// Watch update_engine over the device's own logcat, if it also runs the
		// MDM client — the otautil app itself never reports install progress.
		h.watchLegacyInstall(serial, depID)
	}
}

// LegacyUpdateStatus accepts the optional phase report (newer otautil builds).
func (h *Handler) LegacyUpdateStatus(w http.ResponseWriter, r *http.Request) {
	req, ok := h.legacyIdentify(w, r, false)
	if !ok {
		return
	}
	pct := 0
	if req.Progress != nil {
		pct = *req.Progress
	}
	phase := strings.ToLower(strings.TrimSpace(req.Phase))
	switch phase {
	case "downloading", "installing", "verifying", "finalizing":
		ota.Legacy.Set(req.SerialNumber, phase, pct)
		st := "installing"
		if phase == "downloading" {
			st = "downloading"
		}
		_ = h.db.SetLegacyOTADeviceStatus(r.Context(), req.SerialNumber, st, "", "")
	case "installed", "success", "need_reboot", "updated_need_reboot":
		// update_engine applied the payload to the inactive slot. The device runs the
		// new build only after a reboot, so this is its own state: the work is done,
		// the switch is pending.
		ota.Legacy.Set(req.SerialNumber, "awaiting_reboot", 100)
		_ = h.db.SetLegacyOTADeviceStatus(r.Context(), req.SerialNumber, "awaiting_reboot", "", "")
		if id := h.legacyDeploymentFor(r.Context(), req.SerialNumber); id > 0 {
			_ = h.db.SetLegacyDeploymentDevice(r.Context(), id, req.SerialNumber, "awaiting_reboot", 100, "", "")
		}
	case "error", "failed":
		ota.Legacy.Clear(req.SerialNumber)
		_ = h.db.SetLegacyOTADeviceStatus(r.Context(), req.SerialNumber, "failed", "", req.Error)
		if id := h.legacyDeploymentFor(r.Context(), req.SerialNumber); id > 0 {
			_ = h.db.SetLegacyDeploymentDevice(r.Context(), id, req.SerialNumber, "failed", -1, req.Error, "")
		}
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown phase"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}


// legacyDeploymentFor is the deployment currently owed to this serial, or 0.
func (h *Handler) legacyDeploymentFor(ctx context.Context, serial string) int {
	dep, _, err := h.db.ResolveLegacyDeployment(ctx, serial)
	if err != nil || dep == nil {
		return 0
	}
	return dep.ID
}
