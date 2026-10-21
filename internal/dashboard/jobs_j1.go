package dashboard

import (
	"fmt"
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Jobs J1 (picked 21 Oct 2026, from the Jobs demos): three count cards (In flight, Needs
// a look, Done today), the Overview's Rollouts timeline for the last seven days, filter
// pills (state, type, sent by), then the live job list as a table with a progress bar
// and a status chip per job. The list, its buckets and its live refresh are unchanged.

// jobsCardInputs is the three triage lists in full (not the capped preview the list
// draws) with their delivery summaries.
type jobsCardInputs struct {
	Attn, Prog, Done []db.Command
	Sum              map[uuid.UUID]db.CommandDeliverySummary
	Now              time.Time
}

// jobsDay is the start of t's day in now's location.
func jobsDay(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

// buildJobsCards builds the In flight, Needs a look and Done today cards. Each line is
// the last seven days by the day a job was sent: jobs sent, jobs with a failure, jobs
// finished.
func buildJobsCards(in jobsCardInputs) []ovStat {
	loc := in.Now.Location()
	today := jobsDay(in.Now, loc)
	yesterday := today.AddDate(0, 0, -1)
	idx := func(t time.Time) int { // 0 = six days ago … 6 = today, -1 = older
		d := int(today.Sub(jobsDay(t, loc)).Hours() / 24)
		if d < 0 || d > 6 {
			return -1
		}
		return 6 - d
	}
	var sent, failed, done [7]float64
	failToday, failYest := 0, 0
	for _, set := range [][]db.Command{in.Attn, in.Prog, in.Done} {
		for _, c := range set {
			if i := idx(c.CreatedAt); i >= 0 {
				sent[i]++
				if in.Sum[c.ID].Failed > 0 {
					failed[i]++
				}
			}
			if in.Sum[c.ID].Failed > 0 {
				switch day := jobsDay(c.CreatedAt, loc); {
				case day.Equal(today):
					failToday++
				case day.Equal(yesterday):
					failYest++
				}
			}
		}
	}
	doneToday, doneYest, okDev, failDev := 0, 0, 0, 0
	for _, c := range in.Done {
		if i := idx(c.CreatedAt); i >= 0 {
			done[i]++
		}
		switch day := jobsDay(c.CreatedAt, loc); {
		case day.Equal(today):
			doneToday++
			okDev += in.Sum[c.ID].Completed
			failDev += in.Sum[c.ID].Failed
		case day.Equal(yesterday):
			doneYest++
		}
	}

	waiting, affected := 0, 0
	for _, c := range in.Prog {
		s := in.Sum[c.ID]
		waiting += s.Pending + s.Delivered
	}
	for _, c := range in.Attn {
		affected += in.Sum[c.ID].Failed
	}

	fl := ovStat{Title: "In flight", Href: "/jobs/history?status=inprogress"}
	fl.A = ovMetric{Value: fmt.Sprint(len(in.Prog)), Label: "job" + sIfN(len(in.Prog)) + " running", Tone: "flat"}
	fl.B = ovMetric{Value: fmt.Sprint(waiting), Label: "device" + sIfN(waiting) + " still to report", Tone: "flat"}
	fl.LineA, _, fl.Fill = ovLines(sent[:], nil)

	na := ovStat{Title: "Needs a look", Href: "/jobs/history?status=attention"}
	na.A = ovMetric{Value: fmt.Sprint(len(in.Attn)), Label: "failed or expired", Tone: "flat"}
	na.A.Delta, na.A.Tone = ovDelta(float64(failToday-failYest), "", true, 0)
	if na.A.Delta != "" {
		na.A.Delta += " today"
	}
	na.B = ovMetric{Value: fmt.Sprint(affected), Label: "device" + sIfN(affected) + " affected", Tone: "flat"}
	na.LineA, _, na.Fill = ovLines(failed[:], nil)

	dt := ovStat{Title: "Done today", Href: "/jobs/history?status=completed"}
	dt.A = ovMetric{Value: fmt.Sprint(doneToday), Label: "job" + sIfN(doneToday) + " finished", Tone: "flat"}
	dt.A.Delta, dt.A.Tone = ovDelta(float64(doneToday-doneYest), "", false, 0)
	if dt.A.Delta != "" {
		dt.A.Delta += " vs yesterday"
	}
	dt.B = ovMetric{Value: "—", Label: "devices succeeded", Tone: "flat"}
	if n := okDev + failDev; n > 0 {
		dt.B.Value = fmt.Sprintf("%d%%", okDev*100/n)
	}
	dt.LineA, _, dt.Fill = ovLines(done[:], nil)
	return []ovStat{fl, na, dt}
}

// jobFilters is what the filter pills offer: the types and senders among the jobs on the
// page (the list shows a capped preview of each bucket, and the pills filter those rows).
type jobFilters struct {
	Types   []ovOpt // Label = type label, Href = the type key
	Senders []ovOpt // Label = who, Href = the data-by value
}

// jobsSenderKey is a row's data-by value: the sender, or "auto" for a system-sent job.
func jobsSenderKey(by string) string {
	if by == "" {
		return "auto"
	}
	return by
}

func buildJobFilters(sets ...[]db.Command) jobFilters {
	var f jobFilters
	types, senders := map[string]bool{}, map[string]bool{}
	for _, set := range sets {
		for _, c := range set {
			if !types[c.Type] {
				types[c.Type] = true
				f.Types = append(f.Types, ovOpt{Label: cmdTypeLabel(c.Type), Href: c.Type})
			}
			k := jobsSenderKey(c.CreatedBy)
			if !senders[k] {
				senders[k] = true
				l := c.CreatedBy
				if k == "auto" {
					l = "Sent automatically"
				}
				f.Senders = append(f.Senders, ovOpt{Label: l, Href: k})
			}
		}
	}
	sort.SliceStable(f.Types, func(i, j int) bool { return f.Types[i].Label < f.Types[j].Label })
	sort.SliceStable(f.Senders, func(i, j int) bool { return f.Senders[i].Label < f.Senders[j].Label })
	return f
}

// jobsJ1Data adds the cards, the rollouts and the filter options to /jobs. The rollouts
// are fleet-wide, so a viewer who sees only part of the fleet does not get them (the jobs
// themselves are already filtered to what the viewer may see).
func (h *Handler) jobsJ1Data(r *http.Request, data map[string]any) {
	attn, _ := data["AttnAll"].([]db.Command)
	prog, _ := data["ProgAll"].([]db.Command)
	done, _ := data["DoneAll"].([]db.Command)
	sum, _ := data["Summaries"].(map[uuid.UUID]db.CommandDeliverySummary)
	now := time.Now()
	data["JobCards"] = buildJobsCards(jobsCardInputs{Attn: attn, Prog: prog, Done: done, Sum: sum, Now: now})
	if r.URL.Query().Get("partial") != "" {
		return
	}
	shownA, _ := data["Attention"].([]db.Command)
	shownP, _ := data["InProgress"].([]db.Command)
	shownD, _ := data["Completed"].([]db.Command)
	data["JobFilters"] = buildJobFilters(shownA, shownP, shownD)
	if h.access(r).hidesDevices() {
		return
	}
	rs, err := h.db.RecentRollouts(r.Context(), 7)
	if err != nil {
		log.Printf("[jobs] rollouts: %v", err)
		return
	}
	data["JobRollouts"] = buildOvRollouts(rs, now, roleCanAppLibrary(h.role(r)))
}
