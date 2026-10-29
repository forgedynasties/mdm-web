package dashboard

import (
	"strconv"
	"strings"
	"time"

	"mdm/internal/db"
)

// Settings › People & sign-in (S1, picked 21 Oct 2026): every server-wide setting about
// accounts in one tab, as a card of rows in the Venue settings style (a row shows its
// value; a form opens in place, a switch acts in place). Moved here from General
// (session length, sign everyone out), Actions (shell, remote control, reason required)
// and Devices (customer reports); access profiles are linked. The rail lists who is
// signed in now and which roles each switch reaches.

type s1Person struct {
	Name, Username, Initial string
	Seen                    string // "now", "5m ago"
	Sessions                int
}

type s1View struct {
	SessionLen  string
	SignedIn    []s1Person
	More        int
	Profiles    string // "Look only · Look and fix · Manage apps too"
	ProfilesN   int
	RemoteRoles string // who holds remote control without a rule
	ShellRoles  string
}

type s1Inputs struct {
	TimeoutSecs int
	SignedIn    []db.SignedIn
	Names       map[string]string // username → display name
	Profiles    []db.AccessProfile
	Now         time.Time
}

// s1Duration says a session length the way people say it: "24 hours", "30 minutes", "7 days".
func s1Duration(secs int) string {
	d := time.Duration(secs) * time.Second
	switch {
	case secs <= 0:
		return "not set"
	case d%(24*time.Hour) == 0 && d > 48*time.Hour:
		return dp1Plural(int(d/(24*time.Hour)), "day", "days")
	case d%time.Hour == 0:
		return dp1Plural(int(d/time.Hour), "hour", "hours")
	case d%time.Minute == 0:
		return dp1Plural(int(d/time.Minute), "minute", "minutes")
	}
	return strconv.Itoa(secs) + " seconds"
}

// s1Holders lists who holds a sensitive action without a rule, top role first.
func s1Holders(action string) string {
	out := []string{"Super admins"}
	for _, d := range roleDefs {
		if d.Key != "admin" && !d.Legacy && sensitiveByDefault(d.Key, action) {
			out = append(out, d.Plural)
		}
	}
	return joinAnd(out) + ", plus anyone a rule names"
}

func buildSettingsS1(in s1Inputs) s1View {
	v := s1View{SessionLen: s1Duration(in.TimeoutSecs), ProfilesN: len(in.Profiles),
		RemoteRoles: s1Holders("remote"), ShellRoles: s1Holders("shell")}
	names := []string{}
	for _, p := range in.Profiles {
		names = append(names, p.Name)
	}
	v.Profiles = strings.Join(names, " · ")
	for i, s := range in.SignedIn {
		if i == 8 {
			v.More = len(in.SignedIn) - 8
			break
		}
		n := in.Names[s.Username]
		if n == "" {
			n = s.Username
		}
		seen := "now"
		if in.Now.Sub(s.LastSeen) >= 2*time.Minute {
			seen = dp1Ago(in.Now, s.LastSeen)
		}
		v.SignedIn = append(v.SignedIn, s1Person{Name: n, Username: s.Username, Initial: strings.ToUpper(n[:1]), Seen: seen, Sessions: s.Sessions})
	}
	return v
}
