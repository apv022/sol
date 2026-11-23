package calendar

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Recurrence rules: the RFC 5545 RRULE subset Calendar understands (design/apps/CALENDAR.md
// "Recurrence"). Expansion works on wall dates in the event's zone and turns each into an
// instant with wallTime, so DST never moves a 09:00 meeting. Invalid dates are skipped, never
// clamped. Every loop is bounded, because rules come from feeds and files written by others.

// Rule is a parsed RRULE.
type Rule struct {
	Freq       string // DAILY, WEEKLY, MONTHLY, YEARLY
	Interval   int
	ByDay      []WeekdayNum
	ByMonthDay []int
	ByMonth    []int
	Count      int
	Until      time.Time // zero when none
	UntilDate  bool      // UNTIL was a date (all-day series)
	UntilFloat bool      // UNTIL was a local time without Z (resolved in the event zone)
	Wkst       time.Weekday
}

// WeekdayNum is one BYDAY entry: a weekday with an optional ordinal (2TU, -1FR; 0 means every).
type WeekdayNum struct {
	N   int
	Day time.Weekday
}

var dayCodes = []string{"SU", "MO", "TU", "WE", "TH", "FR", "SA"}

func dayCode(s string) (time.Weekday, bool) {
	for i, c := range dayCodes {
		if c == s {
			return time.Weekday(i), true
		}
	}
	return 0, false
}

const (
	maxPeriods     = 50000 // periods walked per expansion (a daily rule for 137 years)
	maxOccurrences = 5000  // occurrences returned per expansion
)

// ParseRule parses an RRULE value ("FREQ=WEEKLY;BYDAY=MO,WE"), with or without the "RRULE:" prefix.
func ParseRule(s string) (*Rule, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "RRULE:")
	if s == "" {
		return nil, errors.New("empty rule")
	}
	if len(s) > 500 {
		return nil, errors.New("rule too long")
	}
	r := &Rule{Interval: 1, Wkst: time.Monday}
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ";") {
		k, v, ok := strings.Cut(part, "=")
		k = strings.ToUpper(strings.TrimSpace(k))
		v = strings.ToUpper(strings.TrimSpace(v))
		if !ok || v == "" {
			return nil, fmt.Errorf("bad rule part %q", part)
		}
		if seen[k] {
			return nil, fmt.Errorf("%s given twice", k)
		}
		seen[k] = true
		switch k {
		case "FREQ":
			switch v {
			case "DAILY", "WEEKLY", "MONTHLY", "YEARLY":
				r.Freq = v
			default:
				return nil, fmt.Errorf("FREQ=%s is not supported (daily, weekly, monthly or yearly)", v)
			}
		case "INTERVAL":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 1000 {
				return nil, fmt.Errorf("bad INTERVAL %q", v)
			}
			r.Interval = n
		case "COUNT":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 100000 {
				return nil, fmt.Errorf("bad COUNT %q", v)
			}
			r.Count = n
		case "UNTIL":
			switch {
			case len(v) == 8:
				t, err := time.Parse(ridDate, v)
				if err != nil {
					return nil, fmt.Errorf("bad UNTIL %q", v)
				}
				r.Until, r.UntilDate = t, true
			case strings.HasSuffix(v, "Z"):
				t, err := time.Parse(ridTime, v)
				if err != nil {
					return nil, fmt.Errorf("bad UNTIL %q", v)
				}
				r.Until = t
			default:
				t, err := time.Parse("20060102T150405", v)
				if err != nil {
					return nil, fmt.Errorf("bad UNTIL %q", v)
				}
				r.Until, r.UntilFloat = t, true
			}
		case "BYDAY":
			for _, d := range strings.Split(v, ",") {
				d = strings.TrimSpace(d)
				if len(d) < 2 {
					return nil, fmt.Errorf("bad BYDAY %q", d)
				}
				wd, ok := dayCode(d[len(d)-2:])
				if !ok {
					return nil, fmt.Errorf("bad BYDAY %q", d)
				}
				n := 0
				if num := d[:len(d)-2]; num != "" {
					var err error
					if n, err = strconv.Atoi(strings.TrimPrefix(num, "+")); err != nil || n == 0 || n < -53 || n > 53 {
						return nil, fmt.Errorf("bad BYDAY %q", d)
					}
				}
				r.ByDay = append(r.ByDay, WeekdayNum{n, wd})
			}
		case "BYMONTHDAY":
			for _, d := range strings.Split(v, ",") {
				n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(d), "+"))
				if err != nil || n == 0 || n < -31 || n > 31 {
					return nil, fmt.Errorf("bad BYMONTHDAY %q", d)
				}
				r.ByMonthDay = append(r.ByMonthDay, n)
			}
		case "BYMONTH":
			for _, d := range strings.Split(v, ",") {
				n, err := strconv.Atoi(strings.TrimSpace(d))
				if err != nil || n < 1 || n > 12 {
					return nil, fmt.Errorf("bad BYMONTH %q", d)
				}
				r.ByMonth = append(r.ByMonth, n)
			}
		case "WKST":
			wd, ok := dayCode(v)
			if !ok {
				return nil, fmt.Errorf("bad WKST %q", v)
			}
			r.Wkst = wd
		default:
			return nil, fmt.Errorf("%s is not supported in repeat rules", k)
		}
	}
	if r.Freq == "" {
		return nil, errors.New("rule has no FREQ")
	}
	if r.Count > 0 && !r.Until.IsZero() {
		return nil, errors.New("COUNT and UNTIL together")
	}
	if r.Freq != "MONTHLY" && r.Freq != "YEARLY" {
		for _, d := range r.ByDay {
			if d.N != 0 {
				return nil, fmt.Errorf("BYDAY ordinals need FREQ=MONTHLY or YEARLY")
			}
		}
	}
	if r.Freq == "WEEKLY" && len(r.ByMonthDay) > 0 {
		return nil, errors.New("BYMONTHDAY with FREQ=WEEKLY")
	}
	return r, nil
}

// String is the canonical RRULE value.
func (r *Rule) String() string {
	parts := []string{"FREQ=" + r.Freq}
	if r.Interval > 1 {
		parts = append(parts, "INTERVAL="+strconv.Itoa(r.Interval))
	}
	if len(r.ByMonth) > 0 {
		parts = append(parts, "BYMONTH="+joinInts(r.ByMonth))
	}
	if len(r.ByMonthDay) > 0 {
		parts = append(parts, "BYMONTHDAY="+joinInts(r.ByMonthDay))
	}
	if len(r.ByDay) > 0 {
		var ds []string
		for _, d := range r.ByDay {
			s := dayCodes[d.Day]
			if d.N != 0 {
				s = strconv.Itoa(d.N) + s
			}
			ds = append(ds, s)
		}
		parts = append(parts, "BYDAY="+strings.Join(ds, ","))
	}
	if r.Wkst != time.Monday {
		parts = append(parts, "WKST="+dayCodes[r.Wkst])
	}
	switch {
	case r.Count > 0:
		parts = append(parts, "COUNT="+strconv.Itoa(r.Count))
	case !r.Until.IsZero() && r.UntilDate:
		parts = append(parts, "UNTIL="+r.Until.Format(ridDate))
	case !r.Until.IsZero() && r.UntilFloat:
		parts = append(parts, "UNTIL="+r.Until.Format("20060102T150405"))
	case !r.Until.IsZero():
		parts = append(parts, "UNTIL="+r.Until.UTC().Format(ridTime))
	}
	return strings.Join(parts, ";")
}

func joinInts(ns []int) string {
	s := make([]string, len(ns))
	for i, n := range ns {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ",")
}

// shortRule turns the UI's simple choices into rules; anything else must already be a rule.
func shortRule(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "daily":
		return "FREQ=DAILY"
	case "weekdays":
		return "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR"
	case "weekly":
		return "FREQ=WEEKLY"
	case "biweekly":
		return "FREQ=WEEKLY;INTERVAL=2"
	case "monthly":
		return "FREQ=MONTHLY"
	case "yearly":
		return "FREQ=YEARLY"
	case "none", "never":
		return ""
	}
	return strings.TrimSpace(s)
}

// ymd is a wall date.
type ymd struct {
	y int
	m time.Month
	d int
}

func (a ymd) less(b ymd) bool {
	if a.y != b.y {
		return a.y < b.y
	}
	if a.m != b.m {
		return a.m < b.m
	}
	return a.d < b.d
}

func dateOf(t time.Time) ymd { y, m, d := t.Date(); return ymd{y, m, d} }

func (a ymd) addDays(n int) ymd {
	return dateOf(time.Date(a.y, a.m, a.d+n, 12, 0, 0, 0, time.UTC))
}

func (a ymd) weekday() time.Weekday { return time.Date(a.y, a.m, a.d, 12, 0, 0, 0, time.UTC).Weekday() }

func containsInt(ns []int, n int) bool {
	for _, x := range ns {
		if x == n {
			return true
		}
	}
	return false
}

// monthDayOK reports whether day d of the month matches BYMONTHDAY (negatives count from the end).
func (r *Rule) monthDayOK(y int, m time.Month, d int) bool {
	if len(r.ByMonthDay) == 0 {
		return true
	}
	dim := daysIn(y, m)
	for _, n := range r.ByMonthDay {
		if n == d || (n < 0 && dim+1+n == d) {
			return true
		}
	}
	return false
}

func (r *Rule) weekdayOK(wd time.Weekday) bool {
	if len(r.ByDay) == 0 {
		return true
	}
	for _, b := range r.ByDay {
		if b.Day == wd {
			return true
		}
	}
	return false
}

// byDayIn returns the days of [first, last] (one month or one year) matching BYDAY with
// ordinals counted within that span.
func (r *Rule) byDayIn(first ymd, n int) map[ymd]bool {
	out := map[ymd]bool{}
	for _, b := range r.ByDay {
		var all []ymd
		for i := 0; i < n; i++ {
			d := first.addDays(i)
			if d.weekday() == b.Day {
				all = append(all, d)
			}
		}
		switch {
		case b.N == 0:
			for _, d := range all {
				out[d] = true
			}
		case b.N > 0 && b.N <= len(all):
			out[all[b.N-1]] = true
		case b.N < 0 && -b.N <= len(all):
			out[all[len(all)+b.N]] = true
		}
	}
	return out
}

// monthDays are the candidate days of one month for MONTHLY (or YEARLY with BYMONTH).
func (r *Rule) monthDays(y int, m time.Month, startDay int) []ymd {
	dim := daysIn(y, m)
	var out []ymd
	switch {
	case len(r.ByDay) > 0:
		set := r.byDayIn(ymd{y, m, 1}, dim)
		for d := 1; d <= dim; d++ {
			if set[ymd{y, m, d}] && r.monthDayOK(y, m, d) {
				out = append(out, ymd{y, m, d})
			}
		}
	case len(r.ByMonthDay) > 0:
		for d := 1; d <= dim; d++ {
			if r.monthDayOK(y, m, d) {
				out = append(out, ymd{y, m, d})
			}
		}
	default:
		if startDay <= dim {
			out = append(out, ymd{y, m, startDay})
		}
	}
	return out
}

// period returns the candidate wall dates of period i, sorted.
func (r *Rule) period(i int, start ymd) []ymd {
	var out []ymd
	monthOK := func(m time.Month) bool { return len(r.ByMonth) == 0 || containsInt(r.ByMonth, int(m)) }
	switch r.Freq {
	case "DAILY":
		d := start.addDays(i * r.Interval)
		if monthOK(d.m) && r.monthDayOK(d.y, d.m, d.d) && r.weekdayOK(d.weekday()) {
			out = append(out, d)
		}
	case "WEEKLY":
		ws := start.addDays(-((int(start.weekday()) - int(r.Wkst) + 7) % 7)).addDays(7 * r.Interval * i)
		days := r.ByDay
		if len(days) == 0 {
			days = []WeekdayNum{{0, start.weekday()}}
		}
		for _, b := range days {
			d := ws.addDays((int(b.Day) - int(r.Wkst) + 7) % 7)
			if monthOK(d.m) {
				out = append(out, d)
			}
		}
	case "MONTHLY":
		mi := int(start.m) - 1 + i*r.Interval
		y, m := start.y+mi/12, time.Month(mi%12+1)
		if monthOK(m) {
			out = r.monthDays(y, m, start.d)
		}
	case "YEARLY":
		y := start.y + i*r.Interval
		switch {
		case len(r.ByMonth) > 0:
			for _, mm := range r.ByMonth {
				out = append(out, r.monthDays(y, time.Month(mm), start.d)...)
			}
		case len(r.ByDay) > 0:
			n := 365
			if daysIn(y, time.February) == 29 {
				n = 366
			}
			set := r.byDayIn(ymd{y, 1, 1}, n)
			for d := range set {
				if r.monthDayOK(d.y, d.m, d.d) {
					out = append(out, d)
				}
			}
		case len(r.ByMonthDay) > 0:
			for m := time.January; m <= time.December; m++ {
				out = append(out, r.monthDays(y, m, start.d)...)
			}
		default:
			if start.d <= daysIn(y, start.m) {
				out = append(out, ymd{y, start.m, start.d})
			}
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].less(out[b]) })
	// BYDAY lists may name a day twice; keep each date once.
	uniq := out[:0]
	for i, d := range out {
		if i == 0 || d != out[i-1] {
			uniq = append(uniq, d)
		}
	}
	return uniq
}

// Expand returns the starts of the occurrences that begin in [from, to), in order, at most
// limit (0 means maxOccurrences). start is the first occurrence; for timed series its wall time
// in loc is what repeats, for all-day series (allDay) start is a UTC midnight. COUNT counts from
// start whatever the window.
func (r *Rule) Expand(start time.Time, loc *time.Location, allDay bool, from, to time.Time, limit int) []time.Time {
	if limit <= 0 || limit > maxOccurrences {
		limit = maxOccurrences
	}
	if allDay {
		loc = time.UTC
	}
	ws := start.In(loc)
	hh, mm, ss := ws.Clock()
	first := dateOf(ws)
	until := r.Until
	if r.UntilFloat {
		until = wallTime(until.Year(), until.Month(), until.Day(), until.Hour(), until.Minute(), until.Second(), loc)
	}
	if !until.IsZero() && r.UntilDate && !allDay {
		// A date UNTIL on a timed series includes that whole day in the event's zone.
		until = wallTime(until.Year(), until.Month(), until.Day(), 23, 59, 59, loc)
	}
	var out []time.Time
	count := 0
	startDone := false
	// emit handles one occurrence in order and reports whether expansion is over.
	emit := func(t time.Time) bool {
		if !until.IsZero() && t.After(until) {
			return true
		}
		count++
		if r.Count > 0 && count > r.Count {
			return true
		}
		if !t.Before(to) {
			return true
		}
		if !t.Before(from) {
			out = append(out, t)
			if len(out) >= limit {
				return true
			}
		}
		return false
	}
	for i := 0; i < maxPeriods; i++ {
		for _, d := range r.period(i, first) {
			if d.less(first) {
				continue
			}
			var t time.Time
			if allDay {
				t = time.Date(d.y, d.m, d.d, 0, 0, 0, 0, time.UTC)
			} else {
				t = wallTime(d.y, d.m, d.d, hh, mm, ss, loc)
			}
			if t.Before(start) {
				continue
			}
			if !startDone {
				startDone = true
				// The first occurrence is always the start, even when the rule would not produce it.
				if t.After(start) && emit(start) {
					return out
				}
			}
			if emit(t) {
				return out
			}
		}
	}
	if !startDone {
		emit(start)
	}
	return out
}

// countBefore is the number of occurrences that start before t (for turning COUNT into UNTIL
// when a series is split).
func (r *Rule) countBefore(start time.Time, loc *time.Location, allDay bool, t time.Time) int {
	c := *r
	c.Count = 0
	n := len(c.Expand(start, loc, allDay, start, t, maxOccurrences))
	if r.Count > 0 && n > r.Count {
		n = r.Count
	}
	return n
}
