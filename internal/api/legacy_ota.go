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
	"time"

	"mdm/internal/db"
	"mdm/internal/ota"
	"mdm/internal/safehttp"

	"github.com/google/uuid"
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
// A Settings toggle picks who answers: the MDM from its own deployments ("mdm"),
// or the old ota-server container, forwarded verbatim ("passthrough"). The app
// polls every 15 minutes at least (it clamps the configured interval) and never
// reports status on its own, so download progress comes from the bytes streamed
// through /device/pkg, and the rest from the next poll on the new build.

const legacyOTAClient = "otautil"

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

// legacyIdentify validates the body, throttles, and upserts the device as a
// legacy check-in (last seen, build, product from the serial), tagging it so the
// dashboard can show "Legacy OTA".
func (h *Handler) legacyIdentify(w http.ResponseWriter, r *http.Request) (*legacyDeviceReq, uuid.UUID, bool) {
	var req legacyDeviceReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 8*1024)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return nil, uuid.Nil, false
	}
	req.SerialNumber = strings.TrimSpace(req.SerialNumber)
	req.BuildID = strings.TrimSpace(req.BuildID)
	if req.SerialNumber == "" || req.BuildID == "" || len(req.SerialNumber) > maxSerialLen || len(req.BuildID) > maxBuildIDLen {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "serial_number and build_id are required"})
		return nil, uuid.Nil, false
	}
	if h.deviceRateLimited(w, req.SerialNumber) {
		return nil, uuid.Nil, false
	}
	extra, _ := json.Marshal(map[string]any{"ota_client": legacyOTAClient, "ota_client_seen": time.Now().UTC().Format(time.RFC3339)})
	deviceID, _, isNew, err := h.db.UpsertCheckin(r.Context(), req.SerialNumber, req.BuildID, nil, extra, true, "")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return nil, uuid.Nil, false
	}
	if isNew {
		h.notifyDeviceOnboarded(r.Context(), deviceID, req.SerialNumber)
	}
	// A poll on the target build closes the deployment (the app rebooted into it).
	if doneIDs, err := h.db.CompleteUpdatesAtTargetBuild(r.Context(), deviceID, req.BuildID); err == nil {
		for _, uid := range doneIDs {
			_ = h.db.CheckAndCompleteUpdate(r.Context(), uid)
			h.shell.ClearOTAProgress(deviceID)
		}
		if len(doneIDs) > 0 {
			h.hub.PublishDeploymentUpdate()
		}
	}
	return &req, deviceID, true
}

// LegacyPoll is the app's heartbeat.
func (h *Handler) LegacyPoll(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.legacyIdentify(w, r); !ok {
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

// LegacyCheckUpdate answers from the device's pending deployment, with the same
// applicability rules as the agent path (incrementals only from their source build).
func (h *Handler) LegacyCheckUpdate(w http.ResponseWriter, r *http.Request) {
	req, deviceID, ok := h.legacyIdentify(w, r)
	if !ok {
		return
	}
	none := func() { writeJSON(w, http.StatusOK, legacyUpdateResp{UpdateAvailable: false}) }
	upd, err := h.db.ResolveUpdateForDevice(r.Context(), deviceID)
	if err != nil {
		log.Printf("[legacy-ota] ResolveUpdateForDevice %s: %v", req.SerialNumber, err)
		none()
		return
	}
	if upd == nil || upd.OtaPackage == nil {
		none()
		return
	}
	pkg := upd.OtaPackage
	switch {
	case pkg.TargetBuildID == req.BuildID:
		_ = h.db.SetUpdateDeviceStatus(r.Context(), upd.ID, deviceID, "installed")
		_ = h.db.CheckAndCompleteUpdate(r.Context(), upd.ID)
		h.shell.ClearOTAProgress(deviceID)
		none()
		return
	case upd.DeviceStatus == "awaiting_reboot" || upd.DeviceStatus == "reboot_sent":
		// Installed to the inactive slot; the app reboots per its own window.
		none()
		return
	case pkg.Type == "incremental" && pkg.SourceBuildID != req.BuildID:
		none()
		return
	}
	meta, err := h.legacyPayloadMeta(r.Context(), pkg)
	if err != nil {
		log.Printf("[legacy-ota] payload metadata for package %d (%s): %v", pkg.ID, pkg.UpdateURL, err)
		none()
		return
	}
	if upd.DeviceStatus == "pending" {
		_ = h.db.SetUpdateDeviceStatus(r.Context(), upd.ID, deviceID, "downloading")
	}
	h.shell.SetOTAProgress(deviceID, uuid.Nil, "downloading", 0)
	h.hub.PublishDeploymentUpdate()
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	dlURL := scheme + "://" + r.Host + "/device/pkg/" + h.legacyToken(upd.ID, deviceID)
	log.Printf("[legacy-ota] %s on %s -> %s (deployment %d, package %d)", req.SerialNumber, req.BuildID, pkg.TargetBuildID, upd.ID, pkg.ID)
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

// legacyToken binds a download to one deployment and one device: updateID.deviceID.mac.
func (h *Handler) legacyToken(updateID int, deviceID uuid.UUID) string {
	body := strconv.Itoa(updateID) + "." + deviceID.String()
	mac := hmac.New(sha256.New, h.legacy.secret)
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
}

func (h *Handler) legacyParseToken(tok string) (int, uuid.UUID, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return 0, uuid.Nil, false
	}
	uid, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, uuid.Nil, false
	}
	did, err := uuid.Parse(parts[1])
	if err != nil {
		return 0, uuid.Nil, false
	}
	if !hmac.Equal([]byte(h.legacyToken(uid, did)), []byte(tok)) {
		return 0, uuid.Nil, false
	}
	return uid, did, true
}

// LegacyPackage streams the package from its stored URL to the device, honouring
// Range (the app resumes), and turns the bytes sent into download progress.
func (h *Handler) LegacyPackage(w http.ResponseWriter, r *http.Request) {
	updateID, deviceID, ok := h.legacyParseToken(r.PathValue("token"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Same resolution as check-update: the device's current deployment and the
	// package that applies to its build. The token must name that deployment.
	upd, err := h.db.ResolveUpdateForDevice(r.Context(), deviceID)
	if err != nil || upd == nil || upd.OtaPackage == nil || upd.ID != updateID {
		http.NotFound(w, r)
		return
	}
	src := upd.OtaPackage.UpdateURL
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
	// Total object size for the percent: Content-Range total, else Content-Length.
	total := resp.ContentLength
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		if i := strings.LastIndexByte(cr, '/'); i >= 0 {
			if n, err := strconv.ParseInt(cr[i+1:], 10, 64); err == nil {
				total = n
			}
		}
	}
	w.WriteHeader(resp.StatusCode)
	buf := make([]byte, 256*1024)
	sent := start
	lastPct, lastPub := -1, time.Now()
	report := func(pct int, force bool) {
		if pct == lastPct && !force {
			return
		}
		lastPct = pct
		h.shell.SetOTAProgress(deviceID, uuid.Nil, "downloading", pct)
		if force || time.Since(lastPub) > 3*time.Second {
			lastPub = time.Now()
			h.hub.PublishDeploymentUpdate()
		}
	}
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // device went away; it resumes with a Range later
			}
			sent += int64(n)
			if total > 0 {
				report(int(sent*100/total), false)
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
		// status report the install phase is shown from here until the next poll on
		// the new build (or an update-status call) settles it.
		h.shell.SetOTAProgress(deviceID, uuid.Nil, "installing", 0)
		h.hub.PublishDeploymentUpdate()
		log.Printf("[legacy-ota] package %d fully sent to %s", upd.OtaPackage.ID, deviceID)
	}
}

// LegacyUpdateStatus accepts the optional phase report (newer otautil builds).
func (h *Handler) LegacyUpdateStatus(w http.ResponseWriter, r *http.Request) {
	req, deviceID, ok := h.legacyIdentify(w, r)
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
		h.shell.SetOTAProgress(deviceID, uuid.Nil, phase, pct)
	case "installed", "success", "need_reboot", "updated_need_reboot":
		h.shell.SetOTAProgress(deviceID, uuid.Nil, "installed", 100)
		h.afterOtaTerminal(r.Context(), deviceID, "installed", "")
	case "error", "failed":
		h.afterOtaTerminal(r.Context(), deviceID, "error", req.Error)
		h.shell.ClearOTAProgress(deviceID)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown phase"})
		return
	}
	h.hub.PublishDeploymentUpdate()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
