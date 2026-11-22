package tasks

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Dates in Tasks are wall-clock strings, never instants: "2026-11-21" and "09:00" mean that day
// and that time on the device that looks at them, so a due date does not move when a laptop
// crosses a time zone. This file holds the quick-add parser (ui/dates.js is its twin, and both
// pass testdata/dates.json) and recurrence.

const (
	dateLayout = "2006-01-02"
	timeLayout = "15:04"
)

// Parsed is what ParseQuick finds in a quick-add line.
type Parsed struct {
	Title  string `json:"title"`
	Due    string `json:"due,omitempty"`
	Time   string `json:"time,omitempty"`
	Repeat string `json:"repeat,omitempty"`
	Flag   bool   `json:"flag,omitempty"`
	List   string `json:"list,omitempty"`
}

var (
	weekdayNames = []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}
	monthNames   = []string{"january", "february", "march", "april", "may", "june", "july", "august", "september", "october", "november", "december"}

	wdRe    = `(sun(?:day)?|mon(?:day)?|tue(?:s(?:day)?)?|wed(?:nesday)?|thu(?:r(?:s(?:day)?)?)?|fri(?:day)?|sat(?:urday)?)`
	monRe   = `(jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|jun(?:e)?|jul(?:y)?|aug(?:ust)?|sep(?:t(?:ember)?)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)`
	reTag   = regexp.MustCompile(`(?i)(^|\s)#([\w-]+)`)
	reFlag  = regexp.MustCompile(`(^|\s)!(\s|$)`)
	reEvery = regexp.MustCompile(`(?i)\bevery\s+(day|week|month|year|` + wdRe + `)\b`)
	reISO   = regexp.MustCompile(`\b(\d{4})-(\d{2})-(\d{2})\b`)
	reMD    = regexp.MustCompile(`(?i)\b(?:on\s+)?` + monRe + `\.?\s+(\d{1,2})(?:st|nd|rd|th)?\b(?:,?\s+(\d{4})\b)?`)
	reDM    = regexp.MustCompile(`(?i)\b(?:on\s+)?(\d{1,2})(?:st|nd|rd|th)?\s+` + monRe + `\b(?:,?\s+(\d{4})\b)?`)
	reSlash = regexp.MustCompile(`\b(\d{1,2})/(\d{1,2})(?:/(\d{2,4}))?\b`)
	reIn    = regexp.MustCompile(`(?i)\bin\s+(\d+|an?)\s+(minute|min|hour|hr|day|week|month|year)s?\b`)
	reNext  = regexp.MustCompile(`(?i)\bnext\s+(week|month|year)\b`)
	reWD    = regexp.MustCompile(`(?i)\b(?:(on|next|this|by)\s+)?` + wdRe + `\b`)
	reDay   = regexp.MustCompile(`(?i)\b(today|tonight|tomorrow|tmrw)\b`)
	rePart  = regexp.MustCompile(`(?i)\bthis\s+(morning|afternoon|evening)\b`)
	reAMPM  = regexp.MustCompile(`(?i)\b(?:at\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)\b`)
	re24    = regexp.MustCompile(`\b(?:at\s+)?(\d{1,2}):(\d{2})\b`)
	reAtN   = regexp.MustCompile(`(?i)\bat\s+(\d{1,2})\b`)
	reNoon  = regexp.MustCompile(`(?i)\b(?:at\s+)?noon\b`)
	reTail  = regexp.MustCompile(`(?i)\s+(on|at|by|due|in|for)$`)
)

// weekdayIndex maps a name or abbreviation to time.Weekday.
func weekdayIndex(s string) int {
	s = strings.ToLower(s)
	for i, n := range weekdayNames {
		if strings.HasPrefix(n, s[:min(3, len(s))]) {
			return i
		}
	}
	return 0
}

func monthIndex(s string) int {
	s = strings.ToLower(s)[:3]
	for i, n := range monthNames {
		if n[:3] == s {
			return i + 1
		}
	}
	return 1
}

// cut removes the match at loc from s, leaving a space.
func cut(s string, loc []int) string { return s[:loc[0]] + " " + s[loc[1]:] }

func day(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// addMonths adds n months to d, clamping to the end of a shorter month (Jan 31 + 1 month = Feb 28).
func addMonths(d time.Time, n int) time.Time {
	first := time.Date(d.Year(), d.Month(), 1, 0, 0, 0, 0, d.Location()).AddDate(0, n, 0)
	last := first.AddDate(0, 1, -1).Day()
	return time.Date(first.Year(), first.Month(), min(d.Day(), last), 0, 0, 0, 0, d.Location())
}

func validDate(y, m, d int, loc *time.Location) (time.Time, bool) {
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, loc)
	return t, m >= 1 && m <= 12 && t.Day() == d
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

// hour12 reads "9" as a clock hour without am/pm: 7 to 11 is morning, 12 noon, 1 to 6 afternoon.
func hour12(h int) int {
	if h >= 1 && h <= 6 {
		return h + 12
	}
	return h
}

// ParseQuick pulls a date, time, repetition, flag and #list out of a quick-add line; what is
// left is the title. now supplies the zone and "today". It never fails: a line with nothing
// recognised is all title.
func ParseQuick(s string, now time.Time) Parsed {
	orig := strings.TrimSpace(s)
	p := Parsed{}
	rest := " " + orig + " "
	today := day(now)
	var date *time.Time
	hh, mm, haveTime := 0, 0, false
	setTime := func(h, m int) { hh, mm, haveTime = h, m, true }
	defH, defM, haveDef := 0, 0, false // time implied by "tonight" or "this evening": explicit times win
	setDef := func(h, m int) { defH, defM, haveDef = h, m, true }
	setDate := func(t time.Time) { date = &t }

	if m := reTag.FindStringSubmatchIndex(rest); m != nil {
		p.List = rest[m[4]:m[5]]
		rest = cut(rest, m[:2])
	}
	if m := reFlag.FindStringIndex(rest); m != nil {
		p.Flag = true
		rest = cut(rest, m)
	}
	if m := reEvery.FindStringSubmatchIndex(rest); m != nil {
		w := strings.ToLower(rest[m[2]:m[3]])
		switch w {
		case "day":
			p.Repeat = "daily"
			setDate(today)
		case "week":
			p.Repeat = "weekly"
			setDate(today)
		case "month":
			p.Repeat = "monthly"
			setDate(today)
		case "year":
			p.Repeat = "yearly"
			setDate(today)
		default:
			p.Repeat = "weekly"
			setDate(today.AddDate(0, 0, (weekdayIndex(w)-int(today.Weekday())+7)%7))
		}
		rest = cut(rest, m[:2])
	}
	if date == nil {
		switch {
		case matchISO(&rest, &date, now):
		case matchMonthDay(&rest, &date, today):
		case matchSlash(&rest, &date, today):
		case matchIn(&rest, &date, &hh, &mm, &haveTime, now):
		case matchNext(&rest, &date, today):
		case matchWeekday(&rest, &date, today):
		default:
			if m := reDay.FindStringSubmatchIndex(rest); m != nil {
				switch strings.ToLower(rest[m[2]:m[3]]) {
				case "today":
					setDate(today)
				case "tonight":
					setDate(today)
					setDef(20, 0)
				default:
					setDate(today.AddDate(0, 0, 1))
				}
				rest = cut(rest, m[:2])
			} else if m := rePart.FindStringSubmatchIndex(rest); m != nil {
				setDate(today)
				switch strings.ToLower(rest[m[2]:m[3]]) {
				case "morning":
					setDef(9, 0)
				case "afternoon":
					setDef(15, 0)
				default:
					setDef(18, 0)
				}
				rest = cut(rest, m[:2])
			}
		}
	}
	if !haveTime {
		if m := reAMPM.FindStringSubmatchIndex(rest); m != nil && atoi(rest[m[2]:m[3]]) >= 1 && atoi(rest[m[2]:m[3]]) <= 12 {
			h := atoi(rest[m[2]:m[3]]) % 12
			if strings.EqualFold(rest[m[6]:m[7]], "pm") {
				h += 12
			}
			mi := 0
			if m[4] >= 0 {
				mi = atoi(rest[m[4]:m[5]])
			}
			if mi < 60 {
				setTime(h, mi)
				rest = cut(rest, m[:2])
			}
		}
	}
	if !haveTime {
		if m := re24.FindStringSubmatchIndex(rest); m != nil && atoi(rest[m[2]:m[3]]) < 24 && atoi(rest[m[4]:m[5]]) < 60 {
			setTime(atoi(rest[m[2]:m[3]]), atoi(rest[m[4]:m[5]]))
			rest = cut(rest, m[:2])
		}
	}
	if !haveTime {
		if m := reNoon.FindStringIndex(rest); m != nil {
			setTime(12, 0)
			rest = cut(rest, m)
		}
	}
	if !haveTime {
		if m := reAtN.FindStringSubmatchIndex(rest); m != nil && atoi(rest[m[2]:m[3]]) >= 1 && atoi(rest[m[2]:m[3]]) <= 12 {
			setTime(hour12(atoi(rest[m[2]:m[3]])), 0)
			rest = cut(rest, m[:2])
		}
	}
	if !haveTime && haveDef {
		setTime(defH, defM)
	}
	if haveTime && date == nil {
		d := today
		if hh*60+mm <= now.Hour()*60+now.Minute() {
			d = today.AddDate(0, 0, 1)
		}
		date = &d
	}
	title := strings.Join(strings.Fields(rest), " ")
	for {
		t := reTail.ReplaceAllString(title, "")
		if t == title {
			break
		}
		title = t
	}
	if title == "" {
		return Parsed{Title: orig}
	}
	p.Title = title
	if date != nil {
		p.Due = date.Format(dateLayout)
	}
	if haveTime {
		p.Time = time.Date(2000, 1, 1, hh, mm, 0, 0, time.UTC).Format(timeLayout)
	}
	return p
}

func matchISO(rest *string, date **time.Time, now time.Time) bool {
	for _, m := range reISO.FindAllStringSubmatchIndex(*rest, -1) {
		s := *rest
		if t, ok := validDate(atoi(s[m[2]:m[3]]), atoi(s[m[4]:m[5]]), atoi(s[m[6]:m[7]]), now.Location()); ok {
			*date = &t
			*rest = cut(s, m[:2])
			return true
		}
	}
	return false
}

// yearFor picks the year of a date written without one: this year, or next when already past.
func yearFor(m, d int, today time.Time) int {
	y := today.Year()
	if t, ok := validDate(y, m, d, today.Location()); ok && t.Before(today) {
		y++
	}
	return y
}

func matchMonthDay(rest *string, date **time.Time, today time.Time) bool {
	s := *rest
	try := func(re *regexp.Regexp, mi, di int) bool {
		for _, m := range re.FindAllStringSubmatchIndex(s, -1) {
			mon, d := monthIndex(s[m[2*mi]:m[2*mi+1]]), atoi(s[m[2*di]:m[2*di+1]])
			y := 0
			if m[6] >= 0 {
				y = atoi(s[m[6]:m[7]])
			} else {
				y = yearFor(mon, d, today)
			}
			if t, ok := validDate(y, mon, d, today.Location()); ok {
				*date = &t
				*rest = cut(s, m[:2])
				return true
			}
		}
		return false
	}
	return try(reMD, 1, 2) || try(reDM, 2, 1)
}

func matchSlash(rest *string, date **time.Time, today time.Time) bool {
	s := *rest
	for _, m := range reSlash.FindAllStringSubmatchIndex(s, -1) {
		mon, d := atoi(s[m[2]:m[3]]), atoi(s[m[4]:m[5]])
		y := yearFor(mon, d, today)
		if m[6] >= 0 {
			y = atoi(s[m[6]:m[7]])
			if y < 100 {
				y += 2000
			}
		}
		if t, ok := validDate(y, mon, d, today.Location()); ok {
			*date = &t
			*rest = cut(s, m[:2])
			return true
		}
	}
	return false
}

func matchIn(rest *string, date **time.Time, hh, mm *int, haveTime *bool, now time.Time) bool {
	m := reIn.FindStringSubmatchIndex(*rest)
	if m == nil {
		return false
	}
	s := *rest
	n := 1
	if w := strings.ToLower(s[m[2]:m[3]]); w != "a" && w != "an" {
		n = atoi(w)
	}
	today := day(now)
	var t time.Time
	switch u := strings.ToLower(s[m[4]:m[5]]); u {
	case "minute", "min", "hour", "hr":
		d := time.Duration(n) * time.Minute
		if u == "hour" || u == "hr" {
			d = time.Duration(n) * time.Hour
		}
		at := now.Add(d)
		t = day(at)
		*hh, *mm, *haveTime = at.Hour(), at.Minute(), true
	case "day":
		t = today.AddDate(0, 0, n)
	case "week":
		t = today.AddDate(0, 0, 7*n)
	case "month":
		t = addMonths(today, n)
	default:
		t = addMonths(today, 12*n)
	}
	*date = &t
	*rest = cut(s, m[:2])
	return true
}

func matchNext(rest *string, date **time.Time, today time.Time) bool {
	m := reNext.FindStringSubmatchIndex(*rest)
	if m == nil {
		return false
	}
	var t time.Time
	switch strings.ToLower((*rest)[m[2]:m[3]]) {
	case "week":
		t = today.AddDate(0, 0, 7)
	case "month":
		t = addMonths(today, 1)
	default:
		t = addMonths(today, 12)
	}
	*date = &t
	*rest = cut(*rest, m[:2])
	return true
}

// matchWeekday finds "friday", "on fri", "next fri", "this fri", "by fri". A bare abbreviation
// ("sat", "mon") is left alone: it is more likely to be part of the title.
func matchWeekday(rest *string, date **time.Time, today time.Time) bool {
	s := *rest
	for _, m := range reWD.FindAllStringSubmatchIndex(s, -1) {
		prefix := ""
		if m[2] >= 0 {
			prefix = strings.ToLower(s[m[2]:m[3]])
		}
		name := strings.ToLower(s[m[4]:m[5]])
		full := false
		for _, n := range weekdayNames {
			full = full || n == name
		}
		if prefix == "" && !full {
			continue
		}
		diff := (weekdayIndex(name) - int(today.Weekday()) + 7) % 7
		if diff == 0 && prefix != "this" {
			diff = 7
		}
		t := today.AddDate(0, 0, diff)
		*date = &t
		*rest = cut(s, m[:2])
		return true
	}
	return false
}

// validRepeat reports whether r is a recurrence the app knows.
func validRepeat(r string) bool {
	switch r {
	case "", "daily", "weekly", "monthly", "yearly":
		return true
	}
	return false
}

// NextDue is the due date after completing a recurring task due on due (empty means today): the
// first occurrence after both due and today. Monthly and yearly steps count from the original
// day, so Jan 31 goes to Feb 28 and then Mar 31.
func NextDue(due, repeat string, now time.Time) string {
	today := day(now)
	start := today
	if t, err := time.ParseInLocation(dateLayout, due, now.Location()); err == nil {
		start = t
	}
	floor := start
	if today.After(floor) {
		floor = today
	}
	for k := 1; k < 20000; k++ {
		var c time.Time
		switch repeat {
		case "daily":
			c = start.AddDate(0, 0, k)
		case "weekly":
			c = start.AddDate(0, 0, 7*k)
		case "monthly":
			c = addMonths(start, k)
		case "yearly":
			c = addMonths(start, 12*k)
		default:
			return ""
		}
		if c.After(floor) {
			return c.Format(dateLayout)
		}
	}
	return ""
}

// at returns the moment a task with date and time strings is due, in loc; ok is false without both.
func at(due, tm string, loc *time.Location) (time.Time, bool) {
	if due == "" || tm == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(dateLayout+" "+timeLayout, due+" "+tm, loc)
	return t, err == nil
}
