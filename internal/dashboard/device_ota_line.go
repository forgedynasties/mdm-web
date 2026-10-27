package dashboard

import (
	"fmt"

	"mdm/internal/db"
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
