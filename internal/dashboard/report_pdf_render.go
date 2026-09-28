package dashboard

import (
	"bytes"
	"fmt"
	"time"

	"github.com/go-pdf/fpdf"
)

// renderReportPDF draws a venue's week as an A4 landscape PDF: the headline tiles, the
// powered hours per day and one row per device — the same figures, from the same
// venueReport, as the page. Drawn directly rather than printed from HTML, so the server
// needs no browser (see the note at the top of report_pdf.go).
//
// Core Helvetica with the cp1252 translator: no font files to ship, and it covers the
// Latin names venues and nicknames are written in. A character outside it prints as
// "?" rather than failing the render.
func renderReportPDF(venue string, v venueReport, generated time.Time) ([]byte, error) {
	type rgb struct{ r, g, b int }
	var (
		ink    = rgb{0x2c, 0x2c, 0x2b}
		muted  = rgb{0x77, 0x73, 0x6f}
		faint  = rgb{0xa0, 0x9c, 0x98}
		line   = rgb{0xe6, 0xe5, 0xe3}
		zebra  = rgb{0xf7, 0xf7, 0xf6}
		track  = rgb{0xef, 0xee, 0xec}
		coral  = rgb{0xff, 0x65, 0x4f}
		okC    = rgb{0x2f, 0x9e, 0x6a}
		warnC  = rgb{0xd9, 0x8e, 0x1a}
		riskC  = rgb{0xd6, 0x45, 0x3d}
		margin = 12.0
	)

	pdf := fpdf.New("L", "mm", "A4", "")
	pdf.SetMargins(margin, margin, margin)
	pdf.SetAutoPageBreak(false, margin)
	pdf.SetCreator("AIO MDM", false)
	pdf.SetTitle(venue+" weekly device report", true)
	pdf.SetCreationDate(generated)
	pdf.AliasNbPages("{nb}")
	tr := pdf.UnicodeTranslatorFromDescriptor("")

	pageW, pageH := pdf.GetPageSize()
	contentW := pageW - 2*margin
	bottom := pageH - margin - 6 // room for the footer

	text := func(c rgb) { pdf.SetTextColor(c.r, c.g, c.b) }
	fill := func(c rgb) { pdf.SetFillColor(c.r, c.g, c.b) }
	draw := func(c rgb) { pdf.SetDrawColor(c.r, c.g, c.b) }
	font := func(style string, size float64) { pdf.SetFont("Helvetica", style, size) }
	hrs := func(min float64) string { return fmt.Sprintf("%.1f", min/60) }

	pdf.SetFooterFunc(func() {
		pdf.SetY(pageH - margin - 2)
		font("", 7.5)
		text(faint)
		pdf.CellFormat(contentW/2, 4, tr("AIO MDM · generated "+generated.UTC().Format("Mon 2 Jan 2006 15:04")+" UTC"), "", 0, "L", false, 0, "")
		pdf.CellFormat(contentW/2, 4, fmt.Sprintf("Page %d of {nb}", pdf.PageNo()), "", 0, "R", false, 0, "")
	})
	pdf.AddPage()

	// Header.
	font("B", 8)
	text(coral)
	pdf.CellFormat(contentW, 4, "WEEKLY DEVICE REPORT", "", 1, "L", false, 0, "")
	font("B", 20)
	text(ink)
	pdf.CellFormat(contentW, 10, tr(venue), "", 1, "L", false, 0, "")
	font("", 10)
	text(muted)
	sub := fmt.Sprintf("%s · %d device%s reporting", reportRange(v.Win), len(v.DeviceWeeks), plural(len(v.DeviceWeeks)))
	if v.Win.Current {
		sub += " · week in progress, figures still change"
	}
	pdf.CellFormat(contentW, 6, tr(sub), "", 1, "L", false, 0, "")
	pdf.Ln(4)

	// Headline tiles.
	type tile struct{ label, value, unit, note string }
	var tiles []tile
	if m := v.Metrics; m != nil {
		drain := tile{"Battery drained by wireless charging", "—", "", "not enough charging time off the charger yet"}
		if m.HasPadDrain() {
			drain = tile{"Battery drained by wireless charging", fmt.Sprintf("%.2f", m.PadDrainPctPerMin()), "%/min",
				fmt.Sprintf("measured over %s hrs off the charger, all devices", hrs(m.PadDrainMinutes))}
		}
		tiles = []tile{
			{"Average uptime", hrs(v.AvgPoweredMinutes), fmt.Sprintf("/ %d hrs", v.WindowHours()),
				fmt.Sprintf("%d%% of the measured window", m.UptimeFullPct())},
			{"Wireless charging in use", hrs(v.AvgPadMinutes), "hrs", "per device, a customer's phone on the T7 pad"},
			drain,
			{"Plugged in", hrs(v.AvgPluggedMinutes), fmt.Sprintf("/ %d hrs", v.WindowHours()),
				fmt.Sprintf("%d%% of the window", v.AvgPluggedPct)},
		}
	}
	if len(tiles) > 0 {
		const gap, tileH = 4.0, 27.0
		tw := (contentW - gap*float64(len(tiles)-1)) / float64(len(tiles))
		y := pdf.GetY()
		draw(line)
		pdf.SetLineWidth(0.3)
		for i, t := range tiles {
			x := margin + float64(i)*(tw+gap)
			pdf.RoundedRect(x, y, tw, tileH, 2, "1234", "D")
			pdf.SetXY(x+4, y+3.5)
			font("B", 7)
			text(muted)
			pdf.CellFormat(tw-8, 4, tr(upper(t.label)), "", 2, "L", false, 0, "")
			pdf.SetX(x + 4)
			font("", 20)
			text(ink)
			vw := pdf.GetStringWidth(tr(t.value))
			pdf.CellFormat(vw+1, 10, tr(t.value), "", 0, "L", false, 0, "")
			font("", 9)
			text(muted)
			pdf.CellFormat(tw-8-vw-1, 11, tr(t.unit), "", 2, "L", false, 0, "")
			pdf.SetXY(x+4, y+19)
			font("", 7.5)
			pdf.CellFormat(tw-8, 4, tr(t.note), "", 0, "L", false, 0, "")
		}
		pdf.SetY(y + tileH + 7)
	}

	// Powered hours per device, day by day. Every day of the week gets a slot, so a
	// day with no data reads as missing rather than the week reading as shorter.
	{
		font("B", 10.5)
		text(ink)
		pdf.CellFormat(90, 6, "Powered hours per device, by day", "", 0, "L", false, 0, "")
		font("", 8)
		text(muted)
		pdf.CellFormat(contentW-90, 6, "each bar against a 24-hour day", "", 1, "R", false, 0, "")
		pdf.Ln(2)
		byDay := map[string]reportBar{}
		for _, b := range v.Bars {
			byDay[b.Label] = b
		}
		const slots, gap, barH = 7, 6.0, 26.0
		bw := (contentW - gap*(slots-1)) / slots
		y := pdf.GetY()
		for i := 0; i < slots; i++ {
			label := v.Win.From.AddDate(0, 0, i).Format("Mon")
			x := margin + float64(i)*(bw+gap)
			fill(track)
			pdf.RoundedRect(x, y, bw, barH, 1.5, "1234", "F")
			b, ok := byDay[label]
			if ok && b.Pct > 0 {
				h := barH * float64(b.Pct) / 100
				fill(coral)
				pdf.RoundedRect(x, y+barH-h, bw, h, 1.5, "1234", "F")
			}
			pdf.SetXY(x, y+barH+1)
			font("", 7.5)
			text(faint)
			pdf.CellFormat(bw/2, 5, label, "", 0, "L", false, 0, "")
			font("B", 8)
			text(muted)
			val := "—"
			if ok {
				val = fmt.Sprintf("%.1fh", b.Hours)
			}
			pdf.CellFormat(bw/2, 5, tr(val), "", 0, "R", false, 0, "")
		}
		pdf.SetY(y + barH + 11)
	}

	// One row per device.
	font("B", 10.5)
	text(ink)
	pdf.CellFormat(contentW, 6, "Individual device reports", "", 1, "L", false, 0, "")
	pdf.Ln(1)
	cols := []struct {
		title string
		w     float64
		align string
	}{
		{"Serial number", 42, "L"},
		{"Name", 55, "L"},
		{"Uptime", 26, "R"},
		{"% of window", 40, "L"},
		{"Days", 16, "R"},
		{"Wireless charging", 32, "R"},
		{"Battery drain", 30, "R"},
		{"Plugged in", 32, "R"},
	}
	const rowH = 7.0
	header := func() {
		font("B", 7.5)
		text(muted)
		draw(line)
		pdf.SetX(margin)
		for _, c := range cols {
			pdf.CellFormat(c.w, rowH, upper(c.title), "B", 0, c.align, false, 0, "")
		}
		pdf.Ln(-1)
	}
	if len(v.DeviceWeeks) == 0 {
		font("", 9)
		text(muted)
		pdf.MultiCell(contentW, 5, tr("No rolled-up days yet for this week. Daily stats are built by the hourly housekeeping job; this fills in once a device here has reported for a day."), "", "L", false)
	} else {
		header()
		for i, d := range v.DeviceWeeks {
			if pdf.GetY()+rowH > bottom {
				pdf.AddPage()
				header()
			}
			y := pdf.GetY()
			if i%2 == 1 {
				fill(zebra)
				pdf.Rect(margin, y, contentW, rowH, "F")
			}
			pdf.SetX(margin)
			font("", 8.5)
			text(ink)
			pdf.CellFormat(cols[0].w, rowH, d.Serial, "", 0, "L", false, 0, "")
			text(muted)
			pdf.CellFormat(cols[1].w, rowH, clip(pdf, tr(d.Nickname), cols[1].w-2), "", 0, "L", false, 0, "")
			font("B", 8.5)
			text(ink)
			pdf.CellFormat(cols[2].w, rowH, hrs(d.PoweredMinutes)+" hrs", "", 0, "R", false, 0, "")

			// Uptime meter, coloured by the same bands as the page.
			pct := d.UptimeFullPct()
			mx := pdf.GetX() + 4
			mw := cols[3].w - 16
			fill(track)
			pdf.RoundedRect(mx, y+rowH/2-1, mw, 2, 1, "1234", "F")
			switch band(pct) {
			case "risk":
				fill(riskC)
			case "warn":
				fill(warnC)
			default:
				fill(okC)
			}
			if pct > 0 {
				pdf.RoundedRect(mx, y+rowH/2-1, mw*float64(pct)/100, 2, 1, "1234", "F")
			}
			pdf.SetX(mx + mw)
			font("", 8)
			text(muted)
			pdf.CellFormat(cols[3].w-4-mw, rowH, fmt.Sprintf("%d%%", pct), "", 0, "R", false, 0, "")

			pdf.CellFormat(cols[4].w, rowH, fmt.Sprintf("%d", d.DeviceDays), "", 0, "R", false, 0, "")
			text(ink)
			pdf.CellFormat(cols[5].w, rowH, hrs(d.PadMinutes)+" hrs", "", 0, "R", false, 0, "")
			drain := "—"
			if d.HasPadDrain() {
				drain = fmt.Sprintf("%.2f %%/min", d.PadDrainPctPerMin())
			} else {
				text(faint)
			}
			pdf.CellFormat(cols[6].w, rowH, tr(drain), "", 0, "R", false, 0, "")
			text(ink)
			pdf.CellFormat(cols[7].w, rowH, hrs(d.PluggedMinutes)+" hrs", "", 0, "R", false, 0, "")
			pdf.Ln(-1)
		}
	}

	note := "Every figure is measured over the device-days that actually reported, so a device deployed midweek shortens its own window instead of dragging the venue down. " +
		"Battery drain needs at least 30 minutes of wireless charging with the tablet off its own charger; a dash means that never happened, not that the drain was zero."
	font("", 7.5)
	if pdf.GetY()+14 > bottom {
		pdf.AddPage()
	}
	pdf.Ln(4)
	text(muted)
	pdf.MultiCell(contentW, 3.8, tr(note), "", "L", false)

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// band is the uptime meter's colour, the same thresholds as the page's "band" helper.
func band(pct int) string {
	switch {
	case pct < 70:
		return "risk"
	case pct < 85:
		return "warn"
	}
	return ""
}

// upper is ASCII upper-casing for the small-caps labels, which are all ASCII.
func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 32
		}
	}
	return string(b)
}

// clip shortens an (already translated) string to fit a width, with an ellipsis.
func clip(pdf *fpdf.Fpdf, s string, w float64) string {
	if pdf.GetStringWidth(s) <= w {
		return s
	}
	for len(s) > 0 && pdf.GetStringWidth(s+"...") > w {
		s = s[:len(s)-1]
	}
	return s + "..."
}
