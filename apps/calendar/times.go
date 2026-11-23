package calendar

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // zone names resolve on Android and Windows too, which ship no zoneinfo
)

// Times in Calendar: a timed event is an instant (RFC 3339, with its offset) plus the IANA zone
// its wall time belongs to; recurrence works on that wall time. An all-day event is a pair of
// dates, the same days everywhere. This file parses what actions accept, turns wall times into
// instants across DST (the RFC 5545 rules), and names occurrences (recurrence ids).

const (
	dateLayout = "2006-01-02"
	ridDate    = "20060102"
	ridTime    = "20060102T150405Z"
	wallLayout = "2006-01-02T15:04"
)

var (
	zoneMu    sync.Mutex
	zoneCache = map[string]*time.Location{}
)

// loadZone resolves an IANA zone name, cached. "" and "Local" are the device zone.
func loadZone(name string, local *time.Location) (*time.Location, error) {
	if name == "" || name == "Local" {
		return local, nil
	}
	zoneMu.Lock()
	defer zoneMu.Unlock()
	if l, ok := zoneCache[name]; ok {
		return l, nil
	}
	if len(name) > 64 || strings.Contains(name, "..") {
		return nil, fmt.Errorf("unknown time zone %q", name)
	}
	l, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("unknown time zone %q", name)
	}
	zoneCache[name] = l
	return l, nil
}

// deviceZone is the IANA name of the device's zone when it can be found ($TZ, /etc/localtime),
// else "" (which every reader takes as the device zone).
func deviceZone() string {
	if tz := strings.TrimPrefix(os.Getenv("TZ"), ":"); tz != "" && !strings.HasPrefix(tz, "/") {
		if _, err := time.LoadLocation(tz); err == nil {
			return tz
		}
	}
	if p, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
		if i := strings.Index(p, "zoneinfo/"); i >= 0 {
			name := p[i+len("zoneinfo/"):]
			if _, err := time.LoadLocation(name); err == nil {
				return name
			}
		}
	}
	return ""
}

// wallTime is the instant of a wall-clock time in loc. A time in the spring-forward gap uses
// the offset from before the gap (so 02:30 becomes 03:30); a repeated time takes the first.
func wallTime(y int, m time.Month, d, hh, mm, ss int, loc *time.Location) time.Time {
	naive := time.Date(y, m, d, hh, mm, ss, 0, time.UTC).Unix()
	_, o1 := time.Unix(naive-86400, 0).In(loc).Zone()
	_, o2 := time.Unix(naive+86400, 0).In(loc).Zone()
	match := func(t time.Time) bool {
		yy, mo, dd := t.Date()
		h, mi, s := t.Clock()
		return yy == y && mo == m && dd == d && h == hh && mi == mm && s == ss
	}
	a := time.Unix(naive-int64(o1), 0).In(loc)
	b := time.Unix(naive-int64(o2), 0).In(loc)
	switch ma, mb := match(a), match(b); {
	case ma && mb:
		if b.Before(a) {
			return b
		}
		return a
	case ma:
		return a
	case mb:
		return b
	}
	if o1 != o2 {
		return a // the gap: offset before the transition
	}
	return time.Date(y, m, d, hh, mm, ss, 0, loc)
}

// parseDate reads YYYY-MM-DD as a UTC midnight.
func parseDate(s string) (time.Time, bool) {
	t, err := time.Parse(dateLayout, s)
	return t, err == nil
}

// parseWhen reads an action's time: RFC 3339, a wall time YYYY-MM-DDTHH:MM[:SS] in loc, or a date
// (all day).
func parseWhen(s string, loc *time.Location) (t time.Time, allDay bool, err error) {
	s = strings.TrimSpace(s)
	if d, ok := parseDate(s); ok {
		return d, true, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, false, nil
	}
	for _, l := range []string{"2006-01-02T15:04:05", wallLayout, "2006-01-02 15:04"} {
		if w, err := time.ParseInLocation(l, s, time.UTC); err == nil {
			h, mi, sec := w.Clock()
			return wallTime(w.Year(), w.Month(), w.Day(), h, mi, sec, loc), false, nil
		}
	}
	return time.Time{}, false, fmt.Errorf("%q is not a time: use 2026-11-24T10:00, an RFC 3339 time or a date", s)
}

// ridOf names an occurrence by its original start.
func ridOf(t time.Time, allDay bool) string {
	if allDay {
		return t.Format(ridDate)
	}
	return t.UTC().Format(ridTime)
}

// parseRID reads a recurrence id back.
func parseRID(s string) (time.Time, bool, bool) {
	if t, err := time.Parse(ridTime, s); err == nil {
		return t, false, true
	}
	if t, err := time.Parse(ridDate, s); err == nil {
		return t, true, true
	}
	return time.Time{}, false, false
}

// validRID reports whether s is a recurrence id.
func validRID(s string) bool { _, _, ok := parseRID(s); return ok }

// sameDay reports whether two times fall on the same date (in their own locations).
func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// daysIn is the number of days of a month.
func daysIn(y int, m time.Month) int { return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day() }
