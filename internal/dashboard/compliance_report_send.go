package dashboard

import (
	"context"
	"fmt"
	"html/template"
	"log"
	"sort"
	"strings"
	"time"

	"mdm/internal/db"
)

// ── Sending the monthly device-security report ────────────────────────────────
//
// On the 1st, from 09:00 venue time, each venue's report for the month just ended is
// emailed to its contact email and to the owner-role users whose access covers it. Only
// while Settings › customer compliance reports is on; once per venue per month however
// often the job runs (compliance_report_sends). A server that was down on the 1st catches
// up until the 3rd; after that the month is skipped rather than arriving out of the blue.
//
// These recipients are customers by design, so the weekly report's @aioapp.com-only rule
// (RestaurantReportEmail) does not apply here: the switch is what holds it back.

const (
	complianceSendHour      = 9
	complianceSendCatchUpTo = 3 // the last day of the month a missed send still goes out
)

// complianceReportDue is the month to send for a venue whose zone is loc, and whether it
// is time: the 1st from 09:00, or the 2nd–3rd for a send that was missed.
func complianceReportDue(now time.Time, loc *time.Location) (string, bool) {
	n := now.In(loc)
	if n.Day() > complianceSendCatchUpTo || (n.Day() == 1 && n.Hour() < complianceSendHour) {
		return "", false
	}
	return lastMonth(now, loc), true
}

// complianceRecipients is the venue contact first, then the owners, each address once
// (case-insensitively), blanks dropped.
func complianceRecipients(contact string, owners []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range append([]string{contact}, owners...) {
		a = strings.TrimSpace(a)
		if a == "" || seen[strings.ToLower(a)] {
			continue
		}
		seen[strings.ToLower(a)] = true
		out = append(out, a)
	}
	return out
}

// RunComplianceReports sends the monthly reports that are due. Safe to run every minute.
func (h *Handler) RunComplianceReports(ctx context.Context) {
	if !h.cfg.ComplianceReportsCustomers() {
		return
	}
	rests, err := h.db.ListRestaurants(ctx)
	if err != nil {
		log.Printf("[compliance-report] venues: %v", err)
		return
	}
	now := time.Now()
	var owners []db.User // loaded once, and only when a venue is due
	ownersLoaded := false
	for i := range rests {
		rest := &rests[i]
		if rest.DeviceCount == 0 {
			continue
		}
		loc := venueLoc(rest)
		month, due := complianceReportDue(now, loc)
		if !due {
			continue
		}
		// Claim first: it is the cheap check that the month has not gone out already, and
		// it keeps a second server from sending it too.
		claimed, err := h.db.ClaimComplianceReportSend(ctx, rest.ID, month)
		if err != nil {
			log.Printf("[compliance-report] claim %s %s: %v", rest.Name, month, err)
			continue
		}
		if !claimed {
			continue
		}
		if !ownersLoaded {
			owners, ownersLoaded = h.complianceOwners(ctx), true
		}
		var ownerMails []string
		for _, u := range owners {
			if rests, _ := h.accessFor(ctx, u.Role, u.Username).visibleCollections(); rests[rest.ID] > 0 {
				ownerMails = append(ownerMails, *u.Email)
			}
		}
		to := complianceRecipients(rest.ContactEmail, ownerMails)
		if len(to) == 0 {
			// Nobody to send it to this month; the claim stays, so it is not retried every
			// minute. A contact added later gets next month's.
			log.Printf("[compliance-report] %s %s: no contact email and no owner covers it", rest.Name, month)
			continue
		}
		from, end, _ := complianceMonth(month, loc, now)
		v, err := h.loadVenueSecurity(ctx, rest, from, end, nil)
		if err != nil {
			log.Printf("[compliance-report] %s %s: %v", rest.Name, month, err)
			_ = h.db.ReleaseComplianceReportSend(ctx, rest.ID, month)
			continue
		}
		url := h.complianceMonthURL(h.publicBase(), rest.ID, month)
		subject := fmt.Sprintf("%s — device security, %s", rest.Name, from.Format("January 2006"))
		body := complianceReportEmailHTML(rest.Name, from, v, url)
		var sent []string
		for _, addr := range to {
			if err := h.mail.Send(ctx, addr, subject, body); err != nil {
				log.Printf("[compliance-report] send %s to %s: %v", rest.Name, addr, err)
				continue
			}
			sent = append(sent, addr)
		}
		if len(sent) == 0 {
			_ = h.db.ReleaseComplianceReportSend(ctx, rest.ID, month) // try again next run
			continue
		}
		_ = h.db.SetComplianceReportRecipients(ctx, rest.ID, month, strings.Join(sent, ", "))
		log.Printf("[compliance-report] %s %s sent to %d recipient(s)", rest.Name, month, len(sent))
	}
}

// complianceOwners is the owner-role users with a verified email.
func (h *Handler) complianceOwners(ctx context.Context) []db.User {
	users, err := h.db.ListUsers(ctx)
	if err != nil {
		log.Printf("[compliance-report] users: %v", err)
		return nil
	}
	var out []db.User
	for _, u := range users {
		if u.Role == "owner" && u.Email != nil && *u.Email != "" && u.EmailVerifiedAt != nil {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out
}

// complianceReportEmailHTML is the monthly email: the three lines and the link to the PDF.
func complianceReportEmailHTML(venue string, month time.Time, v venueSecurity, url string) string {
	const (
		ink    = "#2c2c2b"
		muted  = "#77736f"
		canvas = "#f7f7f6"
		line   = "#e6e5e3"
		coral  = "#ff654f"
		ok     = "#2f9e6a"
		warn   = "#d98e1a"
		font   = "-apple-system,Segoe UI,Roboto,sans-serif"
	)
	esc := template.HTMLEscapeString
	var b strings.Builder
	fmt.Fprintf(&b, `<!doctype html><html><body style="margin:0;padding:0;background:%s;">`, canvas)
	fmt.Fprintf(&b, `<table role="presentation" cellpadding="0" cellspacing="0" border="0" width="100%%" style="background:%s;padding:24px 12px;"><tr><td align="center">`, canvas)
	fmt.Fprintf(&b, `<table role="presentation" cellpadding="0" cellspacing="0" border="0" width="560" style="width:560px;max-width:100%%;background:#ffffff;border:1px solid %s;border-radius:14px;">`, line)
	fmt.Fprintf(&b, `<tr><td style="padding:22px 24px 16px;border-bottom:1px solid %s;">`+
		`<div style="font:600 11px %s;letter-spacing:.11em;text-transform:uppercase;color:%s;">Monthly device security report</div>`+
		`<div style="font:600 24px %s;color:%s;padding:6px 0 0;">%s</div>`+
		`<div style="font:400 13px %s;color:%s;padding:6px 0 0;">%s · %d device%s</div></td></tr>`,
		line, font, coral, font, ink, esc(venue), font, muted, month.Format("January 2006"), v.Devices, plural(v.Devices))
	b.WriteString(`<tr><td style="padding:8px 24px 4px;">`)
	for _, l := range v.Lines {
		mark, colour := "✓", ok
		if !l.AllClean() {
			mark, colour = "!", warn
		}
		count := "not measured"
		if l.DaysMeasured > 0 {
			count = fmt.Sprintf("%d of %d day%s", l.DaysClean, l.DaysMeasured, plural(l.DaysMeasured))
		} else {
			mark, colour = "–", muted
		}
		fmt.Fprintf(&b, `<table role="presentation" width="100%%" cellpadding="0" cellspacing="0" border="0" style="border-bottom:1px solid %s;"><tr>`+
			`<td width="26" style="padding:11px 0;font:700 14px %s;color:%s;">%s</td>`+
			`<td style="padding:11px 0;font:400 14px %s;color:%s;">%s</td>`+
			`<td align="right" style="padding:11px 0;font:400 13px %s;color:%s;white-space:nowrap;">%s</td></tr></table>`,
			line, font, colour, mark, font, ink, esc(l.Text()), font, muted, count)
	}
	fmt.Fprintf(&b, `</td></tr><tr><td style="padding:16px 24px 22px;">`+
		`<a href="%s" style="display:inline-block;background:%s;color:#ffffff;text-decoration:none;font:600 14px %s;padding:10px 18px;border-radius:10px;">Open the report (PDF)</a>`+
		`<div style="font:400 12px %s;line-height:1.5;color:%s;padding:14px 0 0;">The report lists what happened during the month and what was done about it. `+
		`The link opens without a login, so anyone you forward this email to can read it.</div></td></tr>`,
		esc(url), coral, font, font, muted)
	b.WriteString(`</table></td></tr></table></body></html>`)
	return b.String()
}
