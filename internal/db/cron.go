package db

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Minimal standard 5-field cron: "minute hour day-of-month month day-of-week".
// Each field supports "*", a number, a range "a-b", a step "*/s" or "a-b/s", and
// comma-separated lists of those. Day-of-week 0 and 7 both mean Sunday. This avoids
// pulling in a cron dependency; NextCron finds the next matching minute by scanning
// forward (bounded), which is simple and correct for these expressions.

type cronField struct{ bits [64]bool } // index = value; sized to the largest field (minute 0-59)

func (f *cronField) has(v int) bool { return v >= 0 && v < len(f.bits) && f.bits[v] }

// parseCronField parses one field into an allowed-value set within [min,max].
func parseCronField(spec string, min, max int) (cronField, error) {
	var f cronField
	set := func(v int) {
		if v == 7 && max == 6 { // dow: 7 == Sunday == 0
			v = 0
		}
		if v >= min && v <= max {
			f.bits[v] = true
		}
	}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return f, fmt.Errorf("empty cron field element")
		}
		step := 1
		if i := strings.Index(part, "/"); i >= 0 {
			s, err := strconv.Atoi(part[i+1:])
			if err != nil || s <= 0 {
				return f, fmt.Errorf("bad cron step %q", part)
			}
			step = s
			part = part[:i]
		}
		lo, hi := min, max
		switch {
		case part == "*":
			// full range
		case strings.Contains(part, "-"):
			ab := strings.SplitN(part, "-", 2)
			a, err1 := strconv.Atoi(ab[0])
			b, err2 := strconv.Atoi(ab[1])
			if err1 != nil || err2 != nil {
				return f, fmt.Errorf("bad cron range %q", part)
			}
			lo, hi = a, b
		default:
			v, err := strconv.Atoi(part)
			if err != nil {
				return f, fmt.Errorf("bad cron value %q", part)
			}
			lo, hi = v, v
		}
		if lo > hi || lo < min || hi > max+1 { // allow hi==7 for dow (max 6)
			return f, fmt.Errorf("cron value out of range: %q", part)
		}
		for v := lo; v <= hi; v += step {
			set(v)
		}
	}
	return f, nil
}

type cronSchedule struct {
	minute, hour, dom, month, dow cronField
	domRestricted, dowRestricted  bool // whether the field is not "*"
}

// ParseCron validates and parses a 5-field cron expression.
func ParseCron(expr string) (*cronSchedule, error) {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron must have 5 fields (min hour dom month dow), got %d", len(fields))
	}
	var c cronSchedule
	var err error
	if c.minute, err = parseCronField(fields[0], 0, 59); err != nil {
		return nil, err
	}
	if c.hour, err = parseCronField(fields[1], 0, 23); err != nil {
		return nil, err
	}
	if c.dom, err = parseCronField(fields[2], 1, 31); err != nil {
		return nil, err
	}
	if c.month, err = parseCronField(fields[3], 1, 12); err != nil {
		return nil, err
	}
	if c.dow, err = parseCronField(fields[4], 0, 6); err != nil {
		return nil, err
	}
	c.domRestricted = fields[2] != "*"
	c.dowRestricted = fields[4] != "*"
	return &c, nil
}

func (c *cronSchedule) matches(t time.Time) bool {
	if !c.minute.has(t.Minute()) || !c.hour.has(t.Hour()) || !c.month.has(int(t.Month())) {
		return false
	}
	domOK := c.dom.has(t.Day())
	dowOK := c.dow.has(int(t.Weekday()))
	// Standard cron: when BOTH day-of-month and day-of-week are restricted, match if
	// EITHER matches; otherwise both must (the unrestricted one is always true).
	if c.domRestricted && c.dowRestricted {
		return domOK || dowOK
	}
	return domOK && dowOK
}

// NextCron returns the next minute at or after `from`+1min that matches expr. Bounded
// to ~4 years of look-ahead; returns an error for an unparseable or never-matching expr.
func NextCron(expr string, from time.Time) (time.Time, error) {
	c, err := ParseCron(expr)
	if err != nil {
		return time.Time{}, err
	}
	// Start at the next whole minute in the same location as `from`.
	t := from.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(4, 0, 0)
	for t.Before(limit) {
		if c.matches(t) {
			return t, nil
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}, fmt.Errorf("cron %q has no next run within 4 years", expr)
}
