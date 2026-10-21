package dashboard

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"mdm/internal/db"
)

// Individual user I1 (picked 21 Oct 2026): Profile and Manage are one page for whoever
// may manage the person. A banner for this person (invite not used, email unverified,
// quiet for a month, powers beyond the role, access ending soon, a setup the two
// questions can't show), Actions / Devices they see / Access cards, the two access
// questions as answers with Edit (today's editor opens in place), their work and recent
// activity, and a rail: the Account card (a row per setting opening in place) and "Check
// a device". Colleagues who may not manage them keep the plain Profile page.

type i1Answer struct {
	N     string // "1", "2", "+"
	Q, A  string
	Chips []string
	More  int
	Sub   string
}

type i1Inputs struct {
	User       *db.User
	Simple     simpleAccess
	Choices    []simpleProfileView
	Grants     []grantView
	Week       db.ActorWeek
	LastAt     *time.Time
	Invited    *time.Time
	Visible    int // devices they can see
	Venues     int // venues those devices are at
	FleetSize  int
	ActorAdmin bool // the viewer is a super admin (may verify an email)
	CanInvite  bool // the viewer may send the invite again
	Now        time.Time
}

type i1View struct {
	Banner  dp1Banner
	Cards   []ovStat
	Answers []i1Answer
	Status  string // the header's status line
}

func buildUserI1(in i1Inputs) i1View {
	u, now := in.User, in.Now
	id := u.ID.String()
	base := "/users/" + id
	var v i1View

	// Answers.
	sees, chips, more := "", []string{}, 0
	switch {
	case u.Role == "admin":
		sees = "Every device"
	case !in.Simple.OK:
		sees = "A setup the two questions can't show"
	case in.Simple.Every:
		sees = "Every device"
		if in.FleetSize > 0 {
			sees += " · " + dp1Plural(in.FleetSize, "device", "devices") + " now, and any that join"
		}
	case len(in.Simple.Places) == 0:
		sees = "Nothing yet"
	default:
		sees = dp1Plural(in.Visible, "device", "devices")
		for i, p := range in.Simple.Places {
			if i == 4 {
				more = len(in.Simple.Places) - 4
				break
			}
			chips = append(chips, p.Name)
		}
	}
	do := ""
	switch {
	case u.Role == "admin":
		do = "Everything"
	case !in.Simple.OK:
		do = "See Advanced rules"
	case in.Simple.Do == "custom":
		labels := []string{}
		for _, k := range in.Simple.Actions {
			if a, ok := accessActionByKey[k]; ok {
				labels = append(labels, a.Label)
			}
		}
		do = "Custom: " + joinAnd(labels)
		if len(labels) == 0 {
			do = "Custom: nothing picked"
		}
	default:
		for _, c := range in.Choices {
			if c.Value == in.Simple.Do {
				do = c.Name
				if strings.HasPrefix(c.Value, "profile:") {
					do += " · profile"
				}
			}
		}
		if do == "" {
			do = "Everything " + roleArticle(u.Role) + " can"
		}
	}
	limit := "No end date"
	if in.Simple.Expires != nil {
		limit = "Ends " + in.Simple.Expires.Local().Format("2 Jan 15:04")
	}
	if in.Simple.Note != "" {
		limit += " · " + in.Simple.Note
	} else {
		limit += " · no note"
	}
	v.Answers = []i1Answer{
		{N: "1", Q: "Which devices can they see?", A: sees, Chips: chips, More: more},
		{N: "2", Q: "What can they do there?", A: do, Sub: "the most " + roleArticle(u.Role) + " can ever do: " + strings.TrimSuffix(roleEverything(u.Role), ".")},
		{N: "+", Q: "Time limit and note", A: limit},
	}
	if u.Role == "admin" {
		v.Answers = v.Answers[:2]
		v.Answers[1].Sub = "super admins have every permission everywhere"
	}

	// Banner.
	b := dp1Banner{Kicker: u.DisplayName() + " · " + roleLabel(u.Role)}
	var todos []dp1Todo
	if in.Invited != nil {
		t := dp1Todo{Title: "The invite hasn't been used", Sub: "Sent " + dp1Ago(now, *in.Invited) + "; the link works for 7 days", Action: "Account", Href: base + "/profile?open=email"}
		if in.CanInvite {
			t.Action, t.Href, t.Post = "Send it again", "", base+"/invite"
		}
		todos = append(todos, t)
	} else if u.Email != nil && u.EmailVerifiedAt == nil {
		t := dp1Todo{Title: "Email not verified, so they can't sign in", Sub: "Signed up " + u.CreatedAt.Format("2 Jan") + " and hasn't clicked the link", Action: "Account", Href: base + "/profile?open=email"}
		if in.ActorAdmin {
			t.Action, t.Href, t.Post = "Verify", "", base+"/verify-email"
		}
		todos = append(todos, t)
	}
	if in.Invited == nil && ((in.LastAt != nil && now.Sub(*in.LastAt) > p1QuietDays*24*time.Hour) ||
		(in.LastAt == nil && now.Sub(u.CreatedAt) > p1QuietDays*24*time.Hour)) {
		sub := "Never active"
		if in.LastAt != nil {
			sub = "Last active " + in.LastAt.Format("2 Jan")
		}
		todos = append(todos, dp1Todo{Title: "Hasn't done anything in " + strconv.Itoa(p1QuietDays) + " days", Sub: sub + " · remove the account if they've left", Action: "Delete…", Href: base + "/profile?open=delete"})
	}
	var hot []string
	soon := 0
	for _, g := range in.Grants {
		if g.Expired {
			continue
		}
		if g.Effect == "allow" {
			for _, k := range g.Actions {
				if a := accessActionByKey[k]; a.Sensitive && !sensitiveByDefault(u.Role, k) && !containsStr(hot, a.Label) {
					hot = append(hot, a.Label)
				}
			}
		}
		if g.ExpiresAt != nil && g.ExpiresAt.After(now) && g.ExpiresAt.Sub(now) <= 7*24*time.Hour {
			soon++
		}
	}
	if len(hot) > 0 {
		todos = append(todos, dp1Todo{Title: "A rule gives more than " + roleArticle(u.Role) + " gets", Sub: joinAnd(hot) + " · check it is still needed", Action: "Rules", Href: base + "/profile?open=rules#ua-adv"})
	}
	if soon > 0 {
		todos = append(todos, dp1Todo{Title: "Access ends within a week", Sub: dp1Plural(soon, "rule ends", "rules end") + " soon · extend it or let it lapse", Action: "Edit", Href: base + "/profile?open=access#ua-access"})
	}
	if u.Role != "admin" && !in.Simple.OK {
		todos = append(todos, dp1Todo{Title: "A setup the two questions can't show", Sub: in.Simple.Why, Action: "Rules", Href: base + "/profile?open=rules#ua-adv"})
	}
	if len(todos) > ovTodoRows {
		b.More = len(todos) - ovTodoRows
		todos = todos[:ovTodoRows]
	}
	b.Todos = todos
	if n := len(todos) + b.More; n > 0 {
		b.Headline = dp1Plural(n, "thing", "things") + " to look at"
	} else {
		b.Tone = "calm"
		b.Headline = "Sees " + strings.ToLower(sees[:1]) + sees[1:]
		if u.Role == "admin" {
			b.Headline = "Sees and can do everything"
		} else if in.Simple.OK && !in.Simple.Every && len(in.Simple.Places) > 0 {
			b.Headline = "Sees " + dp1Plural(in.Visible, "device", "devices")
		}
		line := []string{}
		if strings.HasSuffix(do, " · profile") {
			line = append(line, "Access comes from the “"+strings.TrimSuffix(do, " · profile")+"” profile")
		} else if u.Role != "admin" {
			line = append(line, "Can do: "+strings.ToLower(do[:1])+do[1:])
		}
		switch {
		case u.Email == nil:
			line = append(line, "No email on the account")
		case u.EmailVerifiedAt != nil:
			line = append(line, "Email verified, signs in normally")
		}
		b.Line = strings.Join(line, ". ") + "."
	}
	v.Banner = b

	// Cards.
	act := ovStat{Title: "Actions", Href: base + "/profile#i1-activity",
		A: ovMetric{Value: strconv.Itoa(in.Week.Actions), Label: "this week", Tone: "flat"},
		B: ovMetric{Value: strconv.Itoa(in.Week.PageViews), Label: "page views", Tone: "flat"}}
	if d := in.Week.Actions - in.Week.PrevActions; d > 0 {
		act.A.Delta, act.A.Tone = fmt.Sprintf("▲ %d vs last week", d), "good"
	} else if d < 0 {
		act.A.Delta = fmt.Sprintf("▼ %d vs last week", -d)
	}
	dev := ovStat{Title: "Devices they see", Href: base + "/profile?open=access#ua-access",
		A: ovMetric{Value: strconv.Itoa(in.Visible), Label: "of " + strconv.Itoa(in.FleetSize), Tone: "flat"},
		B: ovMetric{Value: strconv.Itoa(in.Venues), Label: "venue" + p1S(in.Venues), Tone: "flat"}}
	live := 0
	var first *time.Time
	for _, g := range in.Grants {
		if g.Expired {
			continue
		}
		live++
		if g.ExpiresAt != nil && (first == nil || g.ExpiresAt.Before(*first)) {
			first = g.ExpiresAt
		}
	}
	acc := ovStat{Title: "Access", Href: base + "/profile?open=rules#ua-adv",
		A: ovMetric{Value: strconv.Itoa(live), Label: "rule" + p1S(live), Tone: "flat"},
		B: ovMetric{Value: "—", Label: "never ends", Tone: "flat"}}
	if strings.HasSuffix(do, " · profile") {
		acc.A.Label += " · profile"
	}
	if u.Role == "admin" {
		acc.A.Value, acc.A.Label = "—", "no rules: super admin"
	}
	if first != nil {
		acc.B.Value, acc.B.Label = first.Local().Format("2 Jan"), "first one ends"
	}
	v.Cards = []ovStat{act, dev, acc}

	st := []string{"Joined " + u.CreatedAt.Format("2 Jan 2006")}
	switch {
	case in.Invited != nil:
		st = append(st, "invite sent "+dp1Ago(now, *in.Invited))
	case in.LastAt != nil:
		st = append(st, "last active "+dp1Ago(now, *in.LastAt))
	default:
		st = append(st, "never active")
	}
	st = append(st, dp1Plural(in.Week.Actions, "action", "actions")+" this week")
	v.Status = strings.Join(st, " · ")
	return v
}
