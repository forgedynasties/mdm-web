package api

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

// Save to library (26 Oct 2026): a Standard-client device answering a pull_apk command
// asks for an upload URL, PUTs the installed APK there, then says it is done. The
// dashboard owns the library and the APK store, so it does the work (APKPuller); these
// handlers check the caller is the device the command names.

// APKPuller is implemented by the dashboard.
type APKPuller interface {
	APKPullUploadURL(ctx context.Context, deviceID, commandID uuid.UUID) (url, key string, err error)
	APKPullRegister(ctx context.Context, deviceID, commandID uuid.UUID, key string) (string, error)
}

// SetAPKPuller wires the dashboard's library into the device API.
func (h *Handler) SetAPKPuller(p APKPuller) { h.apkPuller = p }

type apkPullBody struct {
	SerialNumber string `json:"serial_number"`
	CommandID    string `json:"command_id"`
	Key          string `json:"key"`
}

// pullCaller decodes the body and checks the device and command; it writes the error.
func (h *Handler) pullCaller(w http.ResponseWriter, r *http.Request) (*apkPullBody, uuid.UUID, uuid.UUID, bool) {
	var b apkPullBody
	if err := decodeDeviceJSON(r.Body, &b); err != nil || b.SerialNumber == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "serial_number and command_id are required"})
		return nil, uuid.Nil, uuid.Nil, false
	}
	if h.apkPuller == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "not available"})
		return nil, uuid.Nil, uuid.Nil, false
	}
	if h.deviceRateLimited(w, b.SerialNumber) || !h.requireDeviceIdentity(w, r, b.SerialNumber) {
		return nil, uuid.Nil, uuid.Nil, false
	}
	cmdID, err := uuid.Parse(b.CommandID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad command_id"})
		return nil, uuid.Nil, uuid.Nil, false
	}
	device, err := h.db.GetDevice(r.Context(), b.SerialNumber)
	if err != nil || device == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "device not found"})
		return nil, uuid.Nil, uuid.Nil, false
	}
	return &b, device.ID, cmdID, true
}

// APKPullURL is POST /api/v1/apk-pull/url {serial_number, command_id} → {url, key,
// content_type}: where to PUT the APK.
func (h *Handler) APKPullURL(w http.ResponseWriter, r *http.Request) {
	_, deviceID, cmdID, ok := h.pullCaller(w, r)
	if !ok {
		return
	}
	url, key, err := h.apkPuller.APKPullUploadURL(r.Context(), deviceID, cmdID)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": url, "key": key, "content_type": "application/vnd.android.package-archive"})
}

// APKPullDone is POST /api/v1/apk-pull/done {serial_number, command_id, key} →
// {result}: the server reads the upload and adds it to the library. The device acks
// the command with the result (or the error).
func (h *Handler) APKPullDone(w http.ResponseWriter, r *http.Request) {
	b, deviceID, cmdID, ok := h.pullCaller(w, r)
	if !ok {
		return
	}
	res, err := h.apkPuller.APKPullRegister(r.Context(), deviceID, cmdID, b.Key)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"result": res})
}
