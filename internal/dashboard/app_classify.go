package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"mdm/internal/db"
)

// Tells the desktop enroll app which nearby phones are "ours". A phone found on the network
// announces its hardware serial (adb-<SERIAL>-xxxxxx), and our own hardware has a structured
// serial: product + model + variant + sku + batch (that is, a production's SerialPrefix) then a
// 5-digit sequence, 14 characters in all. A production owns a range of sequences, so a serial can
// be matched to the batch it came from without the phone ever having checked in.
//
// Classes, strongest first:
//   fleet       the MDM already has a device with this serial
//   production  not in the MDM yet, but inside a production batch's serial range
//   lookalike   our product + model codes, but no batch covers it (new batch not entered yet, or a typo)
//   other       anything else: not our hardware

const classifyMax = 100

// serialMatchesProduction reports whether a serial falls inside the production's batch and range.
// Same rule as the SQL the productions page uses (14 chars, 5 numeric digits after the prefix).
func serialMatchesProduction(serial string, p db.Production) bool {
	prefix := p.SerialPrefix()
	if len(serial) != 14 || len(prefix)+5 != 14 || !strings.HasPrefix(serial, prefix) {
		return false
	}
	seq, err := strconv.Atoi(serial[len(prefix):])
	if err != nil || strings.ContainsAny(serial[len(prefix):], "+- ") {
		return false
	}
	return seq >= p.StartSequence && seq <= p.EndSequence
}

// serialLooksLikeProduct: 14 characters starting with a production's product + model codes.
func serialLooksLikeProduct(serial string, ps []db.Production) bool {
	if len(serial) != 14 {
		return false
	}
	for _, p := range ps {
		if head := p.ProductCode + p.ModelCode; head != "" && strings.HasPrefix(serial, head) {
			return true
		}
	}
	return false
}

type serialClass struct {
	Class      string `json:"class"`                // fleet | production | lookalike | other
	Production string `json:"production,omitempty"` // batch name, for production / fleet devices that match one
	Model      string `json:"model,omitempty"`      // model code of the batch
	Status     string `json:"status,omitempty"`     // fleet only: enrollment status
	DevClass   string `json:"device_class,omitempty"`
}

// classifySerial is pure so it can be tested without a database. `known` is the MDM device for
// the serial, or nil.
func classifySerial(serial string, known *db.Device, ps []db.Production) serialClass {
	var batch *db.Production
	for i := range ps {
		if serialMatchesProduction(serial, ps[i]) {
			batch = &ps[i]
			break
		}
	}
	switch {
	case known != nil:
		c := serialClass{Class: "fleet", Status: known.EnrollmentStatus, DevClass: known.DeviceClass}
		if batch != nil {
			c.Production, c.Model = batch.Name, batch.ModelCode
		}
		return c
	case batch != nil:
		return serialClass{Class: "production", Production: batch.Name, Model: batch.ModelCode}
	case serialLooksLikeProduct(serial, ps):
		return serialClass{Class: "lookalike"}
	}
	return serialClass{Class: "other"}
}

// AppClassify: POST /api/v1/app/classify {"serials":[...]} → {"serials":{"<serial>":{class,…}}}.
func (h *Handler) AppClassify(w http.ResponseWriter, r *http.Request, _ *db.Session) {
	var body struct {
		Serials []string `json:"serials"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		appErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	seen := map[string]bool{}
	var serials []string
	for _, s := range body.Serials {
		if s = strings.TrimSpace(s); s != "" && !seen[s] {
			seen[s] = true
			serials = append(serials, s)
		}
	}
	if len(serials) == 0 {
		appErr(w, http.StatusBadRequest, "serials is required")
		return
	}
	if len(serials) > classifyMax {
		appErr(w, http.StatusBadRequest, fmt.Sprintf("at most %d serials per call", classifyMax))
		return
	}
	ps, err := h.db.ListProductions(r.Context(), nil)
	if err != nil {
		appErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make(map[string]serialClass, len(serials))
	for _, s := range serials {
		var known *db.Device
		if d, err := h.db.GetDevice(r.Context(), s); err == nil && d != nil {
			known = d
		}
		out[s] = classifySerial(s, known, ps)
	}
	appJSON(w, http.StatusOK, map[string]any{"serials": out})
}
