package dashboard

import (
	"encoding/json"
	"sort"
	"strings"
)

// NuggetID is the Android ID an aio.app.nugget* app sees on a device, as reported by the
// MDM client (extra.app_android_ids; firmware with the settings_ssaid sepolicy only).
type NuggetID struct {
	Package   string `json:"package"`
	AndroidID string `json:"android_id"`
}

// nuggetAndroidIDs reads them out of a device's latest telemetry, sorted by package.
func nuggetAndroidIDs(extra json.RawMessage) []NuggetID {
	if len(extra) == 0 {
		return nil
	}
	var e struct {
		IDs []NuggetID `json:"app_android_ids"`
	}
	if json.Unmarshal(extra, &e) != nil {
		return nil
	}
	out := e.IDs[:0:0]
	for _, id := range e.IDs {
		if strings.TrimSpace(id.AndroidID) != "" && strings.HasPrefix(id.Package, "aio.app.nugget") {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Package < out[j].Package })
	return out
}

// nuggetIDsJSON is the list as JSON, for the header chip to hand to its dialog.
func nuggetIDsJSON(ids []NuggetID) string {
	b, err := json.Marshal(ids)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// nuggetIDsAllSame reports whether every variant has the same ID, which is the case when they
// are signed with the same key. The header then shows the one ID instead of a count.
func nuggetIDsAllSame(ids []NuggetID) bool {
	for _, id := range ids {
		if id.AndroidID != ids[0].AndroidID {
			return false
		}
	}
	return len(ids) > 0
}

// nuggetIDByPackage is the same as a lookup by package name, for the Apps tab.
func nuggetIDByPackage(extra json.RawMessage) map[string]string {
	m := map[string]string{}
	for _, id := range nuggetAndroidIDs(extra) {
		m[id.Package] = id.AndroidID
	}
	return m
}
