package dashboard

import (
	"context"
	"strconv"
	"time"
)

// Users list UL1 (18 Nov 2026): the people table with housekeeping tiles above it. Left:
// People ring and actions per day, By role filter rows. The tiles filter the table in place
// (unverified sign-ups open the waiting list, super admin only).

type ul1View struct {
	Rail      p1Rail
	Unused    int // invites not used
	Quiet     int // nothing in 30 days
	Own       int // people holding grants of their own
	Waiting   int
	Team      int
	Customers int
	Dash      string // stroke-dasharray for the ring
}

func (h *Handler) buildUL1(ctx context.Context, team, customers []userRow, waiting int, now time.Time) ul1View {
	days, _ := h.db.PeopleActivityDaily(ctx, 7)
	v := buildUsersP1(p1Inputs{Team: team, Customers: customers, Waiting: waiting, Days: days, Now: now})
	u := ul1View{Rail: v.Rail, Waiting: waiting, Team: len(team), Customers: len(customers)}
	for _, r := range append(append([]userRow{}, team...), customers...) {
		switch r.Activity {
		case "invited":
			u.Unused++
		case "quiet":
			u.Quiet++
		}
		if r.Own {
			u.Own++
		}
	}
	u.Dash = strconv.FormatFloat(float64(u.Rail.Pct)*2.136, 'f', 1, 64) + " 214"
	return u
}
