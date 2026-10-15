package dashboard

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ClientVersionRow is one row of a client's version table (static/client-pane-demos.html,
// C2, picked 14 Oct): a build, what changed in it, and the devices running it — so
// "what is out there" and "what did each release do" are one list, with the action
// (update the ones behind, roll back to an older build) on the row it applies to.
type ClientVersionRow struct {
	Key     string // the version, or "nobuild" / "unknown" for the two rows without one
	Version string
	Code    int64
	Kind    string // hosted | newer | behind | release | nobuild | unknown
	Title   string // the changelog's lead, or what the row means
	Detail  string // the line under it
	Release *ClientHistoryRow
	Devices []ClientDeviceRow
	// Serials the row's Update button sends to: behind, not mid-update, not on another
	// server. Empty when there is nothing to send.
	Serials  string
	NSend    int
	Updating int
	Pct      int  // share of the client's devices, for the bar
	Old      bool // a release no device runs, past the first few: folded away
}

// changelogPrefix is the "client 1.9.9 (code 82): " the publish scripts put first.
var changelogPrefix = regexp.MustCompile(`^\s*client\s+\S+\s*(\(code\s*\d+\))?\s*:\s*`)

// changelogLead turns a publisher's changelog into a row title: the prefix gone, the
// first clause only, first letter up.
func changelogLead(s string) string {
	s = changelogPrefix.ReplaceAllString(strings.TrimSpace(s), "")
	for _, sep := range []string{" — ", "; ", ". "} {
		if i := strings.Index(s, sep); i > 0 {
			s = s[:i]
		}
	}
	s = strings.TrimRight(s, ". ")
	if r, n := utf8.DecodeRuneInString(s); n > 0 {
		s = string(unicode.ToUpper(r)) + s[n:]
	}
	return s
}

// changelogText is a publisher's changelog for reading in full: the prefix gone, first
// letter up, nothing cut.
func changelogText(s string) string {
	s = strings.TrimSpace(changelogPrefix.ReplaceAllString(strings.TrimSpace(s), ""))
	if r, n := utf8.DecodeRuneInString(s); n > 0 {
		s = string(unicode.ToUpper(r)) + s[n:]
	}
	return s
}

// clientVersionRows groups the open client's devices by the version they run and lays
// the release history over them.
func clientVersionRows(sel *ClientSlotView, hosted string) []ClientVersionRow {
	byVer := map[string]*ClientVersionRow{}
	var nobuild, unknown ClientVersionRow
	nobuild = ClientVersionRow{Key: "nobuild", Kind: "nobuild", Title: "Signed for another firmware tree",
		Detail: "Older T7 images (v2.0) and Kiosk 22s trust another platform key: they need a build signed for their tree"}
	unknown = ClientVersionRow{Key: "unknown", Kind: "unknown", Title: "Never reported a version",
		Detail: "A client older than version reporting: it needs a firmware OTA"}
	for _, r := range sel.Rows {
		switch {
		case r.State == "nobuild":
			nobuild.Devices = append(nobuild.Devices, r)
			continue
		case r.Version == "" || r.Code == 0:
			if r.State != "updating" {
				unknown.Devices = append(unknown.Devices, r)
				continue
			}
		}
		key := r.Version
		if key == "" {
			key = "unknown"
		}
		row := byVer[key]
		if row == nil {
			row = &ClientVersionRow{Key: key, Version: r.Version, Code: r.Code}
			byVer[key] = row
		}
		row.Devices = append(row.Devices, r)
		if r.Code > row.Code {
			row.Code = r.Code
		}
	}
	// Releases: newest publish of each version wins (a re-publish adds a history row).
	for i := range sel.History {
		h := &sel.History[i]
		v := h.Build.Version
		if v == "" {
			continue
		}
		row := byVer[v]
		if row == nil {
			row = &ClientVersionRow{Key: v, Version: v, Code: h.Build.VersionCode}
			byVer[v] = row
		}
		if row.Release == nil || h.Hosted {
			row.Release = h
		}
		if h.Build.VersionCode > row.Code {
			row.Code = h.Build.VersionCode
		}
	}
	total := len(sel.Rows)
	out := make([]ClientVersionRow, 0, len(byVer)+2)
	for _, row := range byVer {
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Code != out[j].Code {
			return out[i].Code > out[j].Code
		}
		return compareVersionNames(out[i].Version, out[j].Version) > 0
	})
	shown := 0
	for i := range out {
		row := &out[i]
		var send []string
		for _, d := range row.Devices {
			if d.State == "updating" {
				row.Updating++
			}
			if d.State == "behind" && d.Elsewhere == "" {
				send = append(send, d.Serial)
			}
		}
		row.Serials, row.NSend = strings.Join(send, ","), len(send)
		switch {
		case row.Release != nil && row.Release.Hosted:
			row.Kind = "hosted"
		case sel.Hosted && row.Code > sel.Build.VersionCode && len(row.Devices) > 0:
			row.Kind = "newer"
		case len(row.Devices) > 0 && sel.Hosted:
			row.Kind = "behind"
		case len(row.Devices) > 0:
			row.Kind = "unknown"
		default:
			row.Kind = "release"
		}
		if row.Release != nil && row.Release.Build.Changelog != "" {
			row.Title = changelogLead(row.Release.Build.Changelog)
		}
		var bits []string
		if row.Release != nil {
			if row.Kind == "hosted" {
				bits = append(bits, "Hosted since "+row.Release.Build.At.Format("2 Jan"))
			} else {
				bits = append(bits, "Published "+row.Release.Build.At.Format("2 Jan"))
			}
		} else {
			bits = append(bits, "Not published here")
		}
		switch row.Kind {
		case "behind":
			if row.NSend > 0 {
				bits = append(bits, "behind "+hosted)
			}
		case "newer":
			bits = append(bits, "newer than the hosted build")
		case "release":
			bits = append(bits, "no devices")
		}
		if row.Updating > 0 {
			bits = append(bits, strconv.Itoa(row.Updating)+" installing now")
		}
		row.Detail = strings.Join(bits, " · ")
		if row.Title == "" {
			row.Title = "No changelog"
		}
		if len(row.Devices) == 0 {
			shown++
			row.Old = shown > 3
		}
		if total > 0 {
			row.Pct = len(row.Devices) * 100 / total
		}
	}
	for _, extra := range []ClientVersionRow{nobuild, unknown} {
		if len(extra.Devices) == 0 {
			continue
		}
		if total > 0 {
			extra.Pct = len(extra.Devices) * 100 / total
		}
		out = append(out, extra)
	}
	return out
}
