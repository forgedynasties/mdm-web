package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

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
//   family      same hardware family as devices already enrolled (learned from them: same
//               serial scheme, manufacturer and model), which also tells us the class to use
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

// serialFamily is a hardware family learned from enrolled devices: their serials share a prefix and
// a length, they report the same manufacturer and model, and (mostly) the same class.
type serialFamily struct {
	Label       string // "SUNMI D2s_KDS_STGL"
	Prefix      string
	Length      int
	DeviceClass string // most common class among the members, "" if none set
	Count       int
}

func fabricatedSerial(s string) bool {
	return strings.HasPrefix(s, "android-") || strings.HasPrefix(s, "msm-") || strings.HasPrefix(s, "unknown-")
}

func commonPrefix(a, b string) string {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return a[:n]
}

// buildFamilies groups devices by manufacturer + model and works out the serial scheme of each
// group. A prefix shorter than 4 characters says nothing, so such groups are dropped. A group of
// one device has no second serial to compare with, so it is trusted only for the first half of its
// serial (at least 4 characters): enough to separate a model, not enough to claim a single unit.
func buildFamilies(samples []db.FamilySample) []serialFamily {
	type grp struct {
		label   string
		serials []string
		classes map[string]int
	}
	groups := map[string]*grp{}
	for _, f := range samples {
		if fabricatedSerial(f.Serial) || strings.TrimSpace(f.Model) == "" {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(f.Manufacturer)) + "|" + strings.ToLower(strings.TrimSpace(f.Model))
		g := groups[key]
		if g == nil {
			g = &grp{label: strings.TrimSpace(strings.TrimSpace(f.Manufacturer) + " " + strings.TrimSpace(f.Model)), classes: map[string]int{}}
			groups[key] = g
		}
		g.serials = append(g.serials, f.Serial)
		if f.DeviceClass != "" {
			g.classes[f.DeviceClass]++
		}
	}
	var out []serialFamily
	for _, g := range groups {
		// Only serials of the group's most common length describe its scheme.
		lens := map[int]int{}
		for _, s := range g.serials {
			lens[len(s)]++
		}
		bestLen, bestN := 0, 0
		for l, n := range lens {
			if n > bestN || (n == bestN && l > bestLen) {
				bestLen, bestN = l, n
			}
		}
		var same []string
		for _, s := range g.serials {
			if len(s) == bestLen {
				same = append(same, s)
			}
		}
		prefix := same[0]
		for _, s := range same[1:] {
			prefix = commonPrefix(prefix, s)
		}
		if len(same) == 1 {
			n := max(4, len(same[0])/2)
			if n > len(same[0]) {
				continue // a serial shorter than 4 characters has no scheme to learn
			}
			prefix = same[0][:n]
		}
		if len(prefix) < 4 {
			continue
		}
		cls, clsN := "", 0
		for c, n := range g.classes {
			if n > clsN || (n == clsN && c < cls) {
				cls, clsN = c, n
			}
		}
		out = append(out, serialFamily{Label: g.label, Prefix: prefix, Length: bestLen, DeviceClass: cls, Count: len(same)})
	}
	return out
}

// matchFamily returns the family whose scheme fits the serial (the longest matching prefix wins).
func matchFamily(serial string, fs []serialFamily) *serialFamily {
	var best *serialFamily
	for i := range fs {
		f := &fs[i]
		if len(serial) == f.Length && strings.HasPrefix(serial, f.Prefix) && (best == nil || len(f.Prefix) > len(best.Prefix)) {
			best = f
		}
	}
	return best
}

type serialClass struct {
	Class      string     `json:"class"`                  // fleet | production | family | lookalike | other
	Name       string     `json:"name,omitempty"`         // fleet only: what the device is ("AIO T7"), from what it reports
	Family     string     `json:"family,omitempty"`       // family only: "SUNMI D2s_KDS_STGL"
	Count      int        `json:"family_count,omitempty"` // family only: how many enrolled devices share it
	Production string     `json:"production,omitempty"`   // batch name, for production / fleet devices that match one
	Model      string     `json:"model,omitempty"`        // model code of the batch
	Status     string     `json:"status,omitempty"`       // fleet only: enrollment status
	DevClass   string     `json:"device_class,omitempty"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"` // fleet only
	Online     bool       `json:"online,omitempty"`       // fleet only: seen within 3× the check-in interval
}

// deviceTitle is the human name of a device from what it reports: "AIO T7", "SUNMI D2s_KDS_STGL".
// The manufacturer is dropped when the model already starts with it.
func deviceTitle(d *db.Device) string {
	mfr := strings.TrimSpace(extraString(d.LatestExtra, "manufacturer"))
	model := strings.TrimSpace(extraString(d.LatestExtra, "model"))
	switch {
	case model == "":
		return ""
	case mfr == "" || strings.HasPrefix(strings.ToLower(model), strings.ToLower(mfr)):
		return model
	}
	return mfr + " " + model
}

// classifySerial is pure so it can be tested without a database. `known` is the MDM device for
// the serial, or nil.
func classifySerial(serial string, known *db.Device, ps []db.Production, fs []serialFamily) serialClass {
	var batch *db.Production
	for i := range ps {
		if serialMatchesProduction(serial, ps[i]) {
			batch = &ps[i]
			break
		}
	}
	switch {
	case known != nil:
		c := serialClass{Class: "fleet", Status: known.EnrollmentStatus, DevClass: known.Class(), Name: deviceTitle(known)}
		if !known.LastSeenAt.IsZero() {
			t := known.LastSeenAt
			c.LastSeenAt = &t
		}
		if batch != nil {
			c.Production, c.Model = batch.Name, batch.ModelCode
		}
		return c
	case batch != nil:
		return serialClass{Class: "production", Production: batch.Name, Model: batch.ModelCode}
	}
	if f := matchFamily(serial, fs); f != nil {
		return serialClass{Class: "family", Family: f.Label, DevClass: f.DeviceClass, Count: f.Count}
	}
	if serialLooksLikeProduct(serial, ps) {
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
	samples, err := h.db.FamilySamples(r.Context())
	if err != nil {
		appErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	fams := buildFamilies(samples)
	out := make(map[string]serialClass, len(serials))
	for _, s := range serials {
		var known *db.Device
		if d, err := h.db.GetDevice(r.Context(), s); err == nil && d != nil {
			known = d
		}
		c := classifySerial(s, known, ps, fams)
		if c.LastSeenAt != nil {
			c.Online = time.Since(*c.LastSeenAt) <= time.Duration(h.cfg.CheckinInterval()*3)*time.Second
		}
		out[s] = c
	}
	appJSON(w, http.StatusOK, map[string]any{"serials": out})
}
