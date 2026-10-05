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

// nuggetIDByPackage is the same as a lookup by package name, for the Apps tab.
func nuggetIDByPackage(extra json.RawMessage) map[string]string {
	m := map[string]string{}
	for _, id := range nuggetAndroidIDs(extra) {
		m[id.Package] = id.AndroidID
	}
	return m
}
