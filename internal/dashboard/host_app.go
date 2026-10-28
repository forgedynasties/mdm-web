package dashboard

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The host app card on the device page (App card, A — picked 16 Oct): the app a device is
// there to run, as the firmware client reports it from outside the app (client 1.11.0+,
// extra.host_app, HostApp.java). The variant in use leads with four numbers; every other
// installed variant is one line under it, since a lab T7 often carries four or five builds
// of the same app pointed at different backends.
//
// The server names the app per role: the role's app on Lineup (role_apps).

type hostAppReport struct {
	Pattern  string           `json:"pattern"`
	Package  string           `json:"package"`
	Variants []hostAppVariant `json:"variants"`
}

type hostAppVariant struct {
	Package     string         `json:"package"`
	VersionName string         `json:"version_name"`
	VersionCode int64          `json:"version_code"`
	InstalledAt int64          `json:"installed_at"`
	UpdatedAt   int64          `json:"updated_at"`
	Installer   string         `json:"installer"`
	Enabled     *bool          `json:"enabled"`
	Running     bool           `json:"running"`
	Foreground  bool           `json:"foreground"`
	PSSMB       *int           `json:"pss_mb"`
	FgTodayMin  *int           `json:"fg_today_min"`
	LastUsedAt  int64          `json:"last_used_at"`
	RxTodayKB   *int64         `json:"rx_today_kb"`
	TxTodayKB   *int64         `json:"tx_today_kb"`
	Exits24h    map[string]int `json:"exits_24h"`
	LastExit    *struct {
		At          int64  `json:"at"`
		Reason      string `json:"reason"`
		Description string `json:"description"`
	} `json:"last_exit"`
}

// HostAppView is what the template draws. Every string is final; the template only lays out.
type HostAppView struct {
	Pattern string
	Count   int
	Active  HostAppLine
	Others  []HostAppLine

	// The variant in use, in detail.
	State     string // "On screen", "Running", "Stopped"
	StateTone string // ok / info / ""
	Today     string // time on screen today, "—" when not reported
	Memory    string
	Problems  int    // crash, ANR, low-memory… exits in the last 24 h
	ProbLabel string // the commonest of them, short ("crash", "LMK")
	ExitsAll  string // every exit reason in the last 24 h, for the tooltip
	Network   string // "38.4 MB ↓ · 2.1 MB ↑", "" when not reported
	LastExit  string // "07:52 · 2h ago", "" when none on record
	LastWhy   string
	LastTone  string
	LastDesc  string
	Installed string
	Updated   string
}

// HostAppLine is one variant as a single line.
type HostAppLine struct {
	Package  string
	Name     string
	Env      string
	EnvTone  string
	Version  string
	Code     int64
	Dot      string // on / run / off
	Note     string
	NoteTone string // tag class when Note is a problem; "" is plain muted text
	Disabled bool
}

// hostAppView reads extra.host_app; nil when the device reports none (an older client, a
// class with no host app) or reports MDM-lite's own host_app, which carries no variants.
func hostAppView(extra json.RawMessage) *HostAppView {
	if len(extra) == 0 {
		return nil
	}
	var probe struct {
		HostApp  *hostAppReport `json:"host_app"`
		Timezone string         `json:"timezone"`
	}
	if json.Unmarshal(extra, &probe) != nil || probe.HostApp == nil || probe.HostApp.Variants == nil {
		return nil
	}
	r := probe.HostApp
	loc := time.UTC
	if probe.Timezone != "" {
		if l, err := time.LoadLocation(probe.Timezone); err == nil {
			loc = l
		}
	}
	v := &HostAppView{Pattern: r.Pattern, Count: len(r.Variants)}
	if len(r.Variants) == 0 {
		return v
	}
	base := strings.TrimSuffix(strings.Split(r.Pattern, ",")[0], "*")

	// The variant in use is the one the client named; the rest keep its order (installed order).
	ai := 0
	for i, x := range r.Variants {
		if x.Package == r.Package {
			ai = i
			break
		}
	}
	a := r.Variants[ai]
	v.Active = hostAppLine(a, base, loc)
	for i, x := range r.Variants {
		if i != ai {
			v.Others = append(v.Others, hostAppLine(x, base, loc))
		}
	}

	switch {
	case a.Foreground:
		v.State, v.StateTone = "On screen", "ok"
	case a.Running:
		v.State, v.StateTone = "Running", "info"
	default:
		v.State = "Stopped"
	}
	v.Today = "—"
	if a.FgTodayMin != nil {
		v.Today = minutesLabel(*a.FgTodayMin)
	}
	v.Memory = "—"
	if a.PSSMB != nil && a.Running {
		v.Memory = fmt.Sprintf("%d MB", *a.PSSMB)
	}
	v.Problems, v.ProbLabel = exitProblems(a.Exits24h)
	v.ExitsAll = exitsSummary(a.Exits24h)
	if a.RxTodayKB != nil || a.TxTodayKB != nil {
		v.Network = fmt.Sprintf("%s ↓ · %s ↑", kbLabel(a.RxTodayKB), kbLabel(a.TxTodayKB))
	}
	if le := a.LastExit; le != nil && le.At > 0 {
		t := time.UnixMilli(le.At).In(loc)
		v.LastExit = hostAppClock(t)
		v.LastWhy = exitReasonLabel(le.Reason)
		v.LastTone = exitTone(le.Reason)
		v.LastDesc = le.Description
	}
	v.Installed = installedLabel(a.InstalledAt, a.Installer, loc)
	if a.UpdatedAt > 0 && a.UpdatedAt-a.InstalledAt > 60_000 {
		v.Updated = hostAppClock(time.UnixMilli(a.UpdatedAt).In(loc))
	}
	return v
}

func hostAppLine(x hostAppVariant, base string, loc *time.Location) HostAppLine {
	env, tone := hostAppEnv(strings.TrimPrefix(strings.TrimPrefix(x.Package, base), "."))
	l := HostAppLine{
		Package: x.Package, Name: hostAppName(base), Env: env, EnvTone: tone,
		Version: x.VersionName, Code: x.VersionCode, Dot: "off",
		Disabled: x.Enabled != nil && !*x.Enabled,
	}
	if l.Version == "" {
		l.Version = "—"
	}
	switch {
	case x.Foreground:
		l.Dot, l.Note = "on", "on screen"
	case x.Running:
		l.Dot, l.Note = "run", "running"
	case x.LastUsedAt > 0:
		l.Note = "used today " + time.UnixMilli(x.LastUsedAt).In(loc).Format("15:04")
	default:
		l.Note = "not used today"
	}
	if n, label := exitProblems(x.Exits24h); n > 0 {
		l.Note, l.NoteTone = fmt.Sprintf("%d %s", n, hostAppPlural(label, n)), "danger"
		if label == "low-memory exit" {
			l.NoteTone = "warn"
		}
	}
	if l.Disabled {
		l.Note, l.NoteTone = "disabled", ""
	}
	return l
}

// hostAppName is the app's own name for the known families; otherwise the package base.
func hostAppName(base string) string {
	switch base {
	case "aio.app.nugget":
		return "Nugget"
	case "aio.app.pos":
		return "AIO POS"
	case "aio.app.menuboards":
		return "Menu board"
	case "aio.app.kiosk":
		return "Kiosk app"
	case "aio.app.kds":
		return "KDS"
	case "aio.app.mpos":
		return "MPOS"
	}
	return base
}

// hostAppEnv names a build variant by its package suffix: aio.app.nugget.uatv2 → UAT v2.
func hostAppEnv(suffix string) (label, tone string) {
	switch strings.ToLower(suffix) {
	case "":
		return "Production", "ok"
	case "prod":
		return "Prod", "ok"
	case "uatv2":
		return "UAT v2", "info"
	case "uat":
		return "UAT", "info"
	case "qa":
		return "QA", "warn"
	case "internal":
		return "Internal", "accent"
	case "it":
		return "IT", "accent"
	case "stagev2":
		return "Stage v2", "v2"
	case "staging", "stage":
		return "Staging", "v2"
	case "pos":
		return "POS", ""
	case "nugget":
		return "Nugget", ""
	}
	return suffix, ""
}

// Exit reasons that mean something went wrong, against the ones that are routine (the user
// swiped it away, Android trimmed an idle process, an update replaced it).
var problemExits = map[string]string{
	"crash":               "crash",
	"native_crash":        "crash",
	"anr":                 "ANR",
	"init_failure":        "crash",
	"low_memory":          "low-memory exit",
	"excessive_resources": "low-memory exit",
}

func exitProblems(m map[string]int) (int, string) {
	total, by := 0, map[string]int{}
	for k, n := range m {
		if label, ok := problemExits[k]; ok {
			total += n
			by[label] += n
		}
	}
	best, bestN := "", 0
	for label, n := range by {
		if n > bestN || (n == bestN && label < best) {
			best, bestN = label, n
		}
	}
	return total, best
}

func exitsSummary(m map[string]int) string {
	if len(m) == 0 {
		return "No exits in the last 24 hours"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return m[keys[i]] > m[keys[j]] || (m[keys[i]] == m[keys[j]] && keys[i] < keys[j]) })
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", m[k], exitReasonLabel(k)))
	}
	return "Last 24 hours: " + strings.Join(parts, ", ")
}

func exitReasonLabel(r string) string {
	switch r {
	case "crash":
		return "crash"
	case "native_crash":
		return "native crash"
	case "anr":
		return "ANR"
	case "low_memory":
		return "low memory"
	case "excessive_resources":
		return "too many resources"
	case "init_failure":
		return "failed to start"
	case "exit_self":
		return "closed itself"
	case "signaled":
		return "killed"
	case "user_requested":
		return "closed by user"
	case "user_stopped":
		return "force-stopped"
	case "dependency_died":
		return "dependency died"
	case "permission_change":
		return "permission changed"
	case "freezer":
		return "frozen"
	case "package_state_change":
		return "app changed"
	case "package_updated":
		return "updated"
	}
	return "trimmed by Android"
}

func exitTone(r string) string {
	switch problemExits[r] {
	case "crash", "ANR":
		return "danger"
	case "low-memory exit":
		return "warn"
	}
	return ""
}

func minutesLabel(m int) string {
	if m < 60 {
		return fmt.Sprintf("%d m", m)
	}
	return fmt.Sprintf("%d h %02d m", m/60, m%60)
}

func kbLabel(kb *int64) string {
	if kb == nil {
		return "—"
	}
	if *kb < 1000 {
		return fmt.Sprintf("%d KB", *kb)
	}
	return fmt.Sprintf("%.1f MB", float64(*kb)/1000)
}

// hostAppClock is a time of day for today, else a date: "07:52", "14 Oct 07:52".
func hostAppClock(t time.Time) string {
	now := time.Now().In(t.Location())
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("15:04")
	}
	return t.Format("2 Jan 15:04")
}

// installedLabel says when and how the app arrived. An install time near 1970 means the
// clock was not set yet: the app came with the device's first boot.
func installedLabel(at int64, installer string, loc *time.Location) string {
	when := "at first boot"
	if at > time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli() {
		when = time.UnixMilli(at).In(loc).Format("2 Jan 2006")
	}
	switch {
	case installer == "":
		return when
	case installer == "com.aioapp.mdm":
		return when + " · by the MDM"
	case strings.Contains(installer, "packageinstaller"):
		return when + " · by hand"
	case installer == "com.android.vending":
		return when + " · from Play"
	case installer == "com.android.shell":
		return when + " · over adb"
	}
	return when + " · by " + installer
}

func hostAppPlural(s string, n int) string {
	if n == 1 || s == "" {
		return s
	}
	if strings.HasSuffix(s, "sh") {
		return s + "es"
	}
	return s + "s"
}
