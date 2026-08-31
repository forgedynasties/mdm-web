package dashboard

import (
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
	"mdm/internal/shell"
)

// demoScenario is one selectable preview of the deployment detail page, built
// entirely from synthetic in-memory data — no DB reads or writes. Lets a design
// review happen against every device-status shape (in progress, all failed, all
// done, etc.) without needing real fleet data in that exact state.
type demoScenario struct {
	Key   string
	Title string
	Desc  string
	Build func() (db.Update, map[string]any) // returns the deployment + its OTA progress map
}

func demoDevice(i int, serialPrefix string) (uuid.UUID, string) {
	return uuid.New(), fmt.Sprintf("%s-%04d", serialPrefix, i)
}

var demoScenarios = []demoScenario{
	{
		Key:   "in-progress",
		Title: "Rolling out",
		Desc:  "Live mix: some downloading with a phase pill, some installed, one failed, a few not started yet.",
		Build: func() (db.Update, map[string]any) {
			now := time.Now()
			var targets []db.UpdateTarget
			progress := map[string]any{}
			add := func(n int, status, buildID string, withProgress *shell.OTAProgress, withErr string, started, completed bool) {
				id, serial := demoDevice(n, "DEMO")
				t := db.UpdateTarget{
					DeviceID: id, SerialNumber: serial, BuildID: buildID,
					Status: status, ErrorCode: withErr, UpdatedAt: now.Add(-time.Duration(n) * time.Minute),
				}
				if started {
					s := now.Add(-6 * time.Minute)
					t.StartedAt = &s
				}
				if completed {
					c := now.Add(-time.Duration(n) * time.Minute)
					t.CompletedAt = &c
				}
				targets = append(targets, t)
				if withProgress != nil {
					progress[id.String()] = withProgress
				}
			}
			add(1, "installed", "2026.06.01-release", nil, "", true, true)
			add(2, "installed", "2026.06.01-release", nil, "", true, true)
			add(3, "downloading", "2026.05.14-release", &shell.OTAProgress{Phase: "downloading", Percent: 62}, "", true, false)
			add(4, "downloading", "2026.05.14-release", &shell.OTAProgress{Phase: "verifying", Percent: 100}, "", true, false)
			add(5, "failed", "2026.05.14-release", nil, "DOWNLOAD_ERROR", true, false)
			add(6, "pending", "2026.05.14-release", nil, "", false, false)
			add(7, "pending", "2026.05.14-release", nil, "", false, false)
			return db.Update{
				ID: 90001, Status: "active", RebootBehavior: "immediate", CreatedAt: now.Add(-24 * time.Minute),
				Targets: targets,
			}, progress
		},
	},
	{
		Key:   "awaiting-reboot",
		Title: "Awaiting reboot",
		Desc:  "Manual reboot behavior: installed and waiting for an operator to apply. Shows the bulk \"Reboot all installed\" action.",
		Build: func() (db.Update, map[string]any) {
			now := time.Now()
			var targets []db.UpdateTarget
			for n := 1; n <= 6; n++ {
				id, serial := demoDevice(n, "DEMO")
				started := now.Add(-40 * time.Minute)
				completed := now.Add(-time.Duration(30+n) * time.Minute)
				status := "awaiting_reboot"
				if n == 6 {
					status = "reboot_sent"
				}
				targets = append(targets, db.UpdateTarget{
					DeviceID: id, SerialNumber: serial, BuildID: "2026.05.14-release",
					Status: status, UpdatedAt: completed, StartedAt: &started, CompletedAt: &completed,
				})
			}
			return db.Update{
				ID: 90002, Status: "active", RebootBehavior: "manual", CreatedAt: now.Add(-50 * time.Minute),
				Targets: targets,
			}, map[string]any{}
		},
	},
	{
		Key:   "failed",
		Title: "Mostly failed",
		Desc:  "Several devices failed with different error codes — shows the retry action and error text per row.",
		Build: func() (db.Update, map[string]any) {
			now := time.Now()
			codes := []string{"DOWNLOAD_ERROR", "VERIFY_ERROR", "INSTALL_ERROR", "DOWNLOAD_ERROR"}
			var targets []db.UpdateTarget
			for n, code := range codes {
				id, serial := demoDevice(n+1, "DEMO")
				started := now.Add(-15 * time.Minute)
				targets = append(targets, db.UpdateTarget{
					DeviceID: id, SerialNumber: serial, BuildID: "2026.04.02-release",
					Status: "failed", ErrorCode: code, UpdatedAt: now.Add(-time.Duration(n) * time.Minute), StartedAt: &started,
				})
			}
			id, serial := demoDevice(5, "DEMO")
			started := now.Add(-15 * time.Minute)
			completed := now.Add(-2 * time.Minute)
			targets = append(targets, db.UpdateTarget{
				DeviceID: id, SerialNumber: serial, BuildID: "2026.05.14-release",
				Status: "installed", UpdatedAt: completed, StartedAt: &started, CompletedAt: &completed,
			})
			return db.Update{
				ID: 90003, Status: "active", RebootBehavior: "immediate", CreatedAt: now.Add(-20 * time.Minute),
				Targets: targets,
			}, map[string]any{}
		},
	},
	{
		Key:   "complete",
		Title: "Complete",
		Desc:  "Finished rollout: every device installed, no live actions offered.",
		Build: func() (db.Update, map[string]any) {
			now := time.Now()
			var targets []db.UpdateTarget
			for n := 1; n <= 10; n++ {
				id, serial := demoDevice(n, "DEMO")
				started := now.Add(-2*time.Hour - time.Duration(n)*time.Minute)
				completed := now.Add(-2*time.Hour + time.Duration(n)*time.Minute)
				targets = append(targets, db.UpdateTarget{
					DeviceID: id, SerialNumber: serial, BuildID: "2026.06.01-release",
					Status: "installed", UpdatedAt: completed, StartedAt: &started, CompletedAt: &completed,
				})
			}
			return db.Update{
				ID: 90004, Status: "complete", RebootBehavior: "immediate", CreatedAt: now.Add(-3 * time.Hour),
				Targets: targets,
			}, map[string]any{}
		},
	},
	{
		Key:   "empty",
		Title: "No targets yet",
		Desc:  "A deployment record with nothing targeted — the empty state.",
		Build: func() (db.Update, map[string]any) {
			return db.Update{
				ID: 90005, Status: "active", RebootBehavior: "scheduled", CreatedAt: time.Now().Add(-2 * time.Minute),
			}, map[string]any{}
		},
	},
}

func findDemoScenario(key string) *demoScenario {
	for i := range demoScenarios {
		if demoScenarios[i].Key == key {
			return &demoScenarios[i]
		}
	}
	return nil
}

// DemoUpdatesIndex lists the available deployment-detail preview scenarios.
func (h *Handler) DemoUpdatesIndex(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "demo_updates.html", map[string]any{
		"Title":     "Demo — Updates",
		"Scenarios": demoScenarios,
	})
}

// DemoUpdatesScenario renders the deployment_detail.html template against synthetic
// data for one scenario — same template real deployments use, so this previews the
// actual page, not a mockup. No DB reads/writes; the "Add targets"/"Edit settings"
// forms are left pointed at a fake deployment ID and will 404 if submitted.
func (h *Handler) DemoUpdatesScenario(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("scenario")
	sc := findDemoScenario(key)
	if sc == nil {
		http.Error(w, "Unknown demo scenario", http.StatusNotFound)
		return
	}
	upd, progress := sc.Build()

	counts := make(map[string]int)
	done := 0
	durSum, durCount := 0, 0
	for _, t := range upd.Targets {
		counts[t.Status]++
		switch t.Status {
		case "installed", "awaiting_reboot", "reboot_sent":
			done++
		}
		if d := t.DurationSeconds(); d >= 0 {
			durSum += d
			durCount++
		}
	}
	avgDuration := -1
	if durCount > 0 {
		avgDuration = durSum / durCount
	}
	var summary []map[string]any
	for _, s := range []string{"pending", "downloading", "installing", "installed", "awaiting_reboot", "reboot_sent", "failed"} {
		if counts[s] > 0 {
			summary = append(summary, map[string]any{"Status": s, "Count": counts[s]})
		}
	}
	pct := 0
	if len(upd.Targets) > 0 {
		pct = done * 100 / len(upd.Targets)
	}
	upd.Release = &db.Release{ID: 90000, Version: "2026.06.01-release", Product: "t7", Status: "published"}

	design := r.URL.Query().Get("design")
	if design != "b" && design != "c" {
		design = "a"
	}

	data := map[string]any{
		"Title":        fmt.Sprintf("Demo — %s", sc.Title),
		"Deployment":   upd,
		"Release":      upd.Release,
		"OTAProgress":  progress,
		"Summary":      summary,
		"SummaryDone":  done,
		"SummaryTotal": len(upd.Targets),
		"SummaryPct":   pct,
		"AvgDuration":  avgDuration,
		"DoneCount":    durCount,
		"Devices":      nil,
		"Online":       map[uuid.UUID]bool{},
		"Groups":       nil,
		"DemoScenario": sc,
		"Design":       design,
	}

	h.render(w, r, "deployment_detail.html", data)
}
