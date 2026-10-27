package dashboard

import (
	"fmt"
	"time"

	"mdm/internal/db"
	"mdm/internal/shell"
)

// The device page's update line (U2, picked 27 Oct 2026): one line instead of the tall
// bar, with the whole update as steps (download, install, verify, reboot: the order the
// firmware client reports, OtaUpdateManager; "finalizing" counts as verify), the package,
// a link to the update and the one action that fits the moment (Cancel while it runs,
// Reboot now once it waits for its reboot).

type otaStep struct {
	Name  string
	State string // done | cur | todo
	Pct   int    // the current step's percent, 0 when not known
}

type otaLine struct {
	Title, Sub   string // "Updating to v2.1.041", "full · 1.2 GB"
	Steps        []otaStep
	Href         string // the update's page
	ReleaseID    int
	UpdateID     int
	Failed       bool
	CanCancel    bool // running: before it is installed
	AwaitsReboot bool
	ETA          string // "2.4 MB/s · 6 min left", "about 3 min left", "estimating…"
}

func otaLineOf(u *db.Update, label string, percent int) otaLine {
	target := "the new build"
	l := otaLine{Href: fmt.Sprintf("/releases/%d/deployments/%d", u.ReleaseID, u.ID), ReleaseID: u.ReleaseID, UpdateID: u.ID}
	if p := u.OtaPackage; p != nil {
		if p.TargetBuildID != "" {
			target = p.TargetBuildID
		}
		l.Sub = p.Type
		if p.Type == "incremental" && p.SourceBuildID != "" {
			l.Sub = "incremental from " + p.SourceBuildID
		}
		if p.PayloadSize > 0 {
			l.Sub += " · " + fmtBytes(p.PayloadSize)
		}
	}
	cur := 0 // index of the current step
	switch label {
	case "Downloading", "Queued":
		cur = 0
	case "Installing":
		cur = 1
	case "Verifying", "Finalizing":
		cur = 2
	case "Awaiting reboot", "Reboot pending", "Installed":
		cur = 3
	}
	switch {
	case u.DeviceStatus == "failed":
		l.Title, l.Failed = "Update to "+target+" failed", true
	case u.DeviceStatus == "awaiting_reboot":
		l.Title, l.AwaitsReboot = "Installed "+target, true
		l.Sub = "waiting for its reboot"
	case u.DeviceStatus == "reboot_sent":
		l.Title = "Rebooting into " + target
		l.Sub = "the build updates at its next check-in"
	default:
		l.Title, l.CanCancel = "Updating to "+target, true
	}
	if l.Failed {
		return l
	}
	for i, n := range []string{"Download", "Install", "Verify", "Reboot"} {
		s := otaStep{Name: n, State: "todo"}
		switch {
		case i < cur:
			s.State = "done"
		case i == cur:
			s.State = "cur"
			if i < 3 && percent > 0 && percent < 100 {
				s.Pct = percent
			}
		}
		l.Steps = append(l.Steps, s)
	}
	return l
}

// otaETAText words the time left in the phase a device is in (27 Oct 2026): the download
// at its current speed (the percent's rate over the package size), installing and
// verifying at the rate update_engine's own percent moves. "" outside a running phase.
func otaETAText(p *shell.OTAProgress, size int64, now time.Time) string {
	if p == nil {
		return ""
	}
	switch p.Phase {
	case "downloading", "installing", "verifying", "finalizing":
	default:
		return ""
	}
	eta, ok := p.ETA(now)
	if !ok {
		return "estimating…"
	}
	left := otaLeft(eta)
	if p.Phase == "downloading" && size > 0 {
		r, _ := p.Rate(now)
		if bps := r * float64(size) / 100; bps > 0 {
			return fmtSpeed(bps) + " · " + left
		}
	}
	return "about " + left
}

func otaLeft(d time.Duration) string {
	if d < time.Minute {
		return "under a minute left"
	}
	m := int(d.Round(time.Minute) / time.Minute)
	switch {
	case m < 60:
		return fmt.Sprintf("%d min left", m)
	}
	return fmt.Sprintf("%d h %02d min left", m/60, m%60)
}

func fmtSpeed(bps float64) string {
	switch {
	case bps >= 1<<20:
		return fmt.Sprintf("%.1f MB/s", bps/(1<<20))
	case bps >= 1<<10:
		return fmt.Sprintf("%.0f KB/s", bps/(1<<10))
	}
	return fmt.Sprintf("%.0f B/s", bps)
}

// otaPkgSize is the size of the package the device was offered, 0 when not known.
func otaPkgSize(u *db.Update) int64 {
	if u != nil && u.OtaPackage != nil {
		return u.OtaPackage.PayloadSize
	}
	return 0
}
