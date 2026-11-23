package calendar

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/apv022/sol/sdk/go/sol"
)

// iCalendar (RFC 5545) export and import (design/apps/CALENDAR.md "Import and export"). Export
// writes VEVENTs with DTSTART/DTEND (TZID wall times, or VALUE=DATE), RRULE, EXDATE, overrides as
// RECURRENCE-ID events, VALARMs and escaped, folded text; VTIMEZONE blocks are not written (every
// common reader resolves IANA TZIDs itself). Import reads the same and more (DURATION, UTC and
// floating times, Windows zone names) and matches events by UID, writing only what differs, so
// importing a file twice changes nothing and two devices importing it converge.

const (
	maxICS       = 8 << 20
	maxICSEvents = 20000
)

// ---------- writing ----------

// icsEscape escapes TEXT values.
func icsEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\r\n", `\n`, "\n", `\n`, "\r", "")
	return r.Replace(s)
}

// fold breaks a content line into 75-octet lines, never inside a UTF-8 sequence.
func fold(line string, sb *strings.Builder) {
	limit := 75
	for len(line) > limit {
		cut := limit
		for cut > 1 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		sb.WriteString(line[:cut])
		sb.WriteString("\r\n ")
		line = line[cut:]
		limit = 74
	}
	sb.WriteString(line)
	sb.WriteString("\r\n")
}

type icsWriter struct{ sb strings.Builder }

func (w *icsWriter) line(s string) { fold(s, &w.sb) }

// when writes a DTSTART/DTEND-like property for an instant or date.
func (w *icsWriter) when(name string, t time.Time, allDay bool, tz string) {
	switch {
	case allDay:
		w.line(name + ";VALUE=DATE:" + t.Format(ridDate))
	case tz != "" && tz != "UTC":
		if l, err := loadZone(tz, time.UTC); err == nil {
			w.line(name + ";TZID=" + tz + ":" + t.In(l).Format("20060102T150405"))
			return
		}
		fallthrough
	default:
		w.line(name + ":" + t.UTC().Format(ridTime))
	}
}

func (w *icsWriter) rid(name, rid string) {
	if _, allDay, ok := parseRID(rid); ok && allDay {
		w.line(name + ";VALUE=DATE:" + rid)
	} else {
		w.line(name + ":" + rid)
	}
}

func icsDuration(minutes int) string {
	if minutes == 0 {
		return "PT0S"
	}
	if minutes%1440 == 0 {
		return "-P" + strconv.Itoa(minutes/1440) + "D"
	}
	return "-PT" + strconv.Itoa(minutes) + "M"
}

// uidOf is an event's iCalendar UID: the stored one, or one made from its id.
func uidOf(e *Event) string {
	if e.UID != "" {
		return e.UID
	}
	return e.ID + "@sol"
}

func (w *icsWriter) event(e *Event, uid, calName, recurrenceID string, stamp time.Time) {
	w.line("BEGIN:VEVENT")
	w.line("UID:" + icsEscape(uid))
	w.line("DTSTAMP:" + stamp.UTC().Format(ridTime))
	if recurrenceID != "" {
		w.rid("RECURRENCE-ID", recurrenceID)
	}
	w.when("DTSTART", e.start, e.AllDay, e.TZ)
	w.when("DTEND", e.end, e.AllDay, e.TZ)
	w.line("SUMMARY:" + icsEscape(e.Title))
	if e.Location != "" {
		w.line("LOCATION:" + icsEscape(e.Location))
	}
	if e.Notes != "" {
		w.line("DESCRIPTION:" + icsEscape(e.Notes))
	}
	if e.rule != nil && recurrenceID == "" {
		w.line("RRULE:" + e.rule.String())
		for _, x := range e.Exdates {
			w.rid("EXDATE", x)
		}
	}
	if calName != "" {
		w.line("X-SOL-CALENDAR:" + icsEscape(calName))
	}
	if e.Color != "" {
		w.line("X-SOL-COLOR:" + e.Color)
	}
	for _, m := range e.Alerts {
		w.line("BEGIN:VALARM")
		w.line("ACTION:DISPLAY")
		w.line("DESCRIPTION:" + icsEscape(e.Title))
		w.line("TRIGGER:" + icsDuration(m))
		w.line("END:VALARM")
	}
	w.line("END:VEVENT")
}

// exportICS writes calendars as one VCALENDAR; with more than one, each event names its calendar.
func (a *App) exportICS(cals []*calState) string {
	w := &icsWriter{}
	w.line("BEGIN:VCALENDAR")
	w.line("VERSION:2.0")
	w.line("PRODID:-//Sol//Calendar//EN")
	w.line("CALSCALE:GREGORIAN")
	if len(cals) == 1 {
		w.line("X-WR-CALNAME:" + icsEscape(cals[0].info.Name))
	}
	stamp := a.clock.Now()
	for _, cs := range cals {
		name := ""
		if len(cals) > 1 {
			name = cs.info.Name
		}
		ids := sortedKeys(cs.events)
		for _, id := range ids {
			e := cs.events[id]
			uid := uidOf(e)
			w.event(e, uid, name, "", stamp)
			for _, rid := range sortedKeys(e.overrides) {
				w.event(e.overrides[rid], uid, name, rid, stamp)
			}
		}
	}
	w.line("END:VCALENDAR")
	return w.sb.String()
}

func (a *App) actExport(_ context.Context, c sol.Call) (any, error) {
	var in struct{ Calendar string }
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	cals := a.view()
	name := "Calendars"
	if in.Calendar != "" {
		cs, err := a.findCal(in.Calendar)
		if err != nil {
			return nil, err
		}
		cals, name = []*calState{cs}, cs.info.Name
	}
	n := 0
	for _, cs := range cals {
		n += len(cs.events)
	}
	return map[string]any{"name": name, "filename": safeFilename(name) + ".ics", "events": n, "ics": a.exportICS(cals)}, nil
}

func safeFilename(s string) string {
	s = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`/\:*?"<>|`, r) || r < 32 {
			return '-'
		}
		return r
	}, s)
	if s == "" {
		return "calendar"
	}
	return s
}

// ---------- reading ----------

type icsProp struct {
	name   string
	params map[string]string
	value  string
}

type icsEvent struct {
	props  []icsProp
	alarms [][]icsProp
}

func (e *icsEvent) get(name string) *icsProp {
	for i := range e.props {
		if e.props[i].name == name {
			return &e.props[i]
		}
	}
	return nil
}

func (e *icsEvent) text(name string) string {
	if p := e.get(name); p != nil {
		return icsUnescape(p.value)
	}
	return ""
}

func icsUnescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 'n', 'N':
				b.WriteByte('\n')
			default:
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// unfold joins continuation lines (a line starting with a space or tab continues the last).
func unfold(src string) []string {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	src = strings.ReplaceAll(src, "\r", "\n")
	var out []string
	for _, l := range strings.Split(src, "\n") {
		if (strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t")) && len(out) > 0 {
			out[len(out)-1] += l[1:]
			continue
		}
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// parseLine splits "NAME;P=V;Q=\"x:y\":value".
func parseLine(l string) (icsProp, bool) {
	p := icsProp{params: map[string]string{}}
	inQ := false
	colon := -1
	for i := 0; i < len(l); i++ {
		switch l[i] {
		case '"':
			inQ = !inQ
		case ':':
			if !inQ {
				colon = i
			}
		}
		if colon >= 0 {
			break
		}
	}
	if colon < 0 {
		return p, false
	}
	head, val := l[:colon], l[colon+1:]
	parts := splitParams(head)
	p.name = strings.ToUpper(parts[0])
	for _, kv := range parts[1:] {
		k, v, _ := strings.Cut(kv, "=")
		p.params[strings.ToUpper(k)] = strings.Trim(v, `"`)
	}
	p.value = val
	return p, p.name != ""
}

func splitParams(s string) []string {
	var out []string
	inQ, start := false, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQ = !inQ
		case ';':
			if !inQ {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// parseICS reads the VEVENTs of a file and the calendar's name.
func parseICS(src string) ([]*icsEvent, string, error) {
	var evs []*icsEvent
	var cur *icsEvent
	var alarm []icsProp
	inAlarm, inCal, seenCal := false, false, false
	calName := ""
	depth := 0 // components other than VEVENT/VALARM inside an event (ignored)
	for _, l := range unfold(src) {
		p, ok := parseLine(l)
		if !ok {
			continue
		}
		switch p.name {
		case "BEGIN":
			v := strings.ToUpper(p.value)
			switch {
			case v == "VCALENDAR":
				inCal, seenCal = true, true
			case v == "VEVENT" && cur == nil && depth == 0:
				cur = &icsEvent{}
			case v == "VALARM" && cur != nil && !inAlarm:
				inAlarm, alarm = true, nil
			default:
				depth++
			}
			continue
		case "END":
			v := strings.ToUpper(p.value)
			switch {
			case v == "VCALENDAR":
				inCal = false
			case v == "VEVENT" && cur != nil && depth == 0:
				if len(evs) >= maxICSEvents {
					return nil, "", bad("more than %d events in one file", maxICSEvents)
				}
				evs = append(evs, cur)
				cur = nil
			case v == "VALARM" && inAlarm:
				cur.alarms = append(cur.alarms, alarm)
				inAlarm = false
			default:
				if depth > 0 {
					depth--
				}
			}
			continue
		}
		switch {
		case depth > 0:
		case inAlarm:
			alarm = append(alarm, p)
		case cur != nil:
			cur.props = append(cur.props, p)
		case inCal && p.name == "X-WR-CALNAME":
			calName = oneLine(icsUnescape(p.value))
		}
	}
	if !seenCal {
		return nil, "", bad("not an iCalendar file (no BEGIN:VCALENDAR)")
	}
	return evs, calName, nil
}

// windowsZones maps the Windows zone names Outlook writes as TZIDs to IANA names.
var windowsZones = map[string]string{
	"Pacific Standard Time": "America/Los_Angeles", "Mountain Standard Time": "America/Denver",
	"Central Standard Time": "America/Chicago", "Eastern Standard Time": "America/New_York",
	"Alaskan Standard Time": "America/Anchorage", "Hawaiian Standard Time": "Pacific/Honolulu",
	"Atlantic Standard Time": "America/Halifax", "E. South America Standard Time": "America/Sao_Paulo",
	"GMT Standard Time": "Europe/London", "Greenwich Standard Time": "Atlantic/Reykjavik",
	"W. Europe Standard Time": "Europe/Berlin", "Romance Standard Time": "Europe/Paris",
	"Central Europe Standard Time": "Europe/Budapest", "Central European Standard Time": "Europe/Warsaw",
	"E. Europe Standard Time": "Europe/Chisinau", "FLE Standard Time": "Europe/Kiev",
	"GTB Standard Time": "Europe/Bucharest", "Russian Standard Time": "Europe/Moscow",
	"Turkey Standard Time": "Europe/Istanbul", "Israel Standard Time": "Asia/Jerusalem",
	"Arabian Standard Time": "Asia/Dubai", "India Standard Time": "Asia/Kolkata",
	"China Standard Time": "Asia/Shanghai", "Singapore Standard Time": "Asia/Singapore",
	"Tokyo Standard Time": "Asia/Tokyo", "Korea Standard Time": "Asia/Seoul",
	"AUS Eastern Standard Time": "Australia/Sydney", "New Zealand Standard Time": "Pacific/Auckland",
	"UTC": "UTC", "Coordinated Universal Time": "UTC",
}

// icsZone resolves a TZID: an IANA name, a Windows name, or a path ending in an IANA name
// (/mozilla.org/.../Europe/Berlin). Unknown ones are the device zone.
func (a *App) icsZone(tzid string) (*time.Location, string) {
	tzid = strings.TrimSpace(tzid)
	if n, ok := windowsZones[tzid]; ok {
		tzid = n
	}
	try := []string{tzid}
	if parts := strings.Split(strings.Trim(tzid, "/"), "/"); len(parts) > 2 {
		try = append(try, strings.Join(parts[len(parts)-2:], "/"), strings.Join(parts[len(parts)-3:], "/"))
	}
	for _, n := range try {
		if n == "" {
			continue
		}
		if l, err := loadZone(n, a.loc); err == nil {
			return l, n
		}
	}
	return a.loc, a.zone
}

// icsTime reads a DATE or DATE-TIME property value: the time, whether it is a date, the zone
// name it belongs to ("" for dates, "UTC" for Z times, the device zone for floating ones).
func (a *App) icsTime(p *icsProp, value string) (time.Time, bool, string, error) {
	v := strings.TrimSpace(value)
	if strings.EqualFold(p.params["VALUE"], "DATE") || len(v) == 8 {
		t, err := time.Parse(ridDate, v)
		return t, true, "", err
	}
	if strings.HasSuffix(v, "Z") {
		t, err := time.Parse(ridTime, v)
		return t, false, "UTC", err
	}
	w, err := time.Parse("20060102T150405", v)
	if err != nil {
		return w, false, "", err
	}
	loc, zone := a.loc, a.zone
	if tz := p.params["TZID"]; tz != "" {
		loc, zone = a.icsZone(tz)
	}
	h, m, s := w.Clock()
	return wallTime(w.Year(), w.Month(), w.Day(), h, m, s, loc), false, zone, nil
}

// icsDurationParse reads [+-]P[nW][nD][T[nH][nM][nS]].
func icsDurationParse(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimLeft(s, "+-")
	if !strings.HasPrefix(s, "P") {
		return 0, errors.New("bad duration")
	}
	s = s[1:]
	var d time.Duration
	inT := false
	num := ""
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
			num += string(c)
		case c == 'T':
			inT = true
		default:
			n, err := strconv.Atoi(num)
			if err != nil || n > 1e6 {
				return 0, errors.New("bad duration")
			}
			num = ""
			switch {
			case c == 'W':
				d += time.Duration(n) * 7 * 24 * time.Hour
			case c == 'D':
				d += time.Duration(n) * 24 * time.Hour
			case c == 'H' && inT:
				d += time.Duration(n) * time.Hour
			case c == 'M' && inT:
				d += time.Duration(n) * time.Minute
			case c == 'S' && inT:
				d += time.Duration(n) * time.Second
			default:
				return 0, errors.New("bad duration")
			}
		}
	}
	if num != "" {
		return 0, errors.New("bad duration")
	}
	if neg {
		d = -d
	}
	return d, nil
}

// importedEvent is one VEVENT resolved into fields.
type importedEvent struct {
	uid, rid, cal string
	f             fields
	exdates       []string
	cancelled     bool
}

// resolve turns a VEVENT into fields; err says why it was skipped.
func (a *App) resolve(ie *icsEvent) (*importedEvent, error) {
	out := &importedEvent{uid: oneLine(ie.text("UID")), cal: oneLine(ie.text("X-SOL-CALENDAR"))}
	out.cancelled = strings.EqualFold(strings.TrimSpace(ie.text("STATUS")), "CANCELLED")
	ds := ie.get("DTSTART")
	if ds == nil {
		return nil, errors.New("no DTSTART")
	}
	start, allDay, zone, err := a.icsTime(ds, ds.value)
	if err != nil {
		return nil, errors.New("bad DTSTART")
	}
	f := fields{Title: oneLine(ie.text("SUMMARY")), Location: ie.text("LOCATION"), Notes: ie.text("DESCRIPTION"), AllDay: allDay, Start: start}
	if f.Title == "" {
		f.Title = "Untitled"
	}
	if len(f.Title) > maxTitle {
		f.Title = f.Title[:maxTitle]
	}
	if c := strings.TrimSpace(ie.text("X-SOL-COLOR")); validColor(c) {
		f.Color = c
	}
	rrule := ""
	if p := ie.get("RRULE"); p != nil {
		if r, err := ParseRule(p.value); err == nil {
			rrule = r.String()
		}
	}
	if !allDay {
		f.TZ = zone
		if zone == "UTC" && rrule == "" {
			f.TZ = a.zone // a one-off in UTC is just an instant: show it in the device zone
		}
	}
	switch de, du := ie.get("DTEND"), ie.get("DURATION"); {
	case de != nil:
		end, _, _, err := a.icsTime(de, de.value)
		if err != nil {
			return nil, errors.New("bad DTEND")
		}
		f.End = end
	case du != nil:
		d, err := icsDurationParse(du.value)
		if err != nil || d < 0 {
			return nil, errors.New("bad DURATION")
		}
		if allDay {
			f.End = start.AddDate(0, 0, max(int(d.Hours()/24), 1))
		} else {
			f.End = start.Add(d)
		}
	default:
		f.End = start
	}
	if allDay {
		f.End = time.Date(f.End.Year(), f.End.Month(), f.End.Day(), 0, 0, 0, 0, time.UTC)
		if !f.End.After(f.Start) {
			f.End = f.Start.AddDate(0, 0, 1)
		}
	}
	if f.End.Before(f.Start) {
		f.End = f.Start
	}
	if p := ie.get("RECURRENCE-ID"); p != nil {
		t, ad, _, err := a.icsTime(p, p.value)
		if err != nil {
			return nil, errors.New("bad RECURRENCE-ID")
		}
		out.rid = ridOf(t, ad)
	} else {
		f.RRule = rrule
		for _, p := range ie.props {
			if p.name != "EXDATE" {
				continue
			}
			for _, v := range strings.Split(p.value, ",") {
				t, ad, _, err := a.icsTime(&p, v)
				if err != nil {
					continue
				}
				if allDay && !ad {
					t, ad = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), true
				}
				out.exdates = append(out.exdates, ridOf(t, ad))
			}
		}
	}
	for _, al := range ie.alarms {
		for _, p := range al {
			if p.name != "TRIGGER" || strings.EqualFold(p.params["VALUE"], "DATE-TIME") || strings.EqualFold(p.params["RELATED"], "END") {
				continue
			}
			d, err := icsDurationParse(p.value)
			if err != nil || d > 0 {
				continue
			}
			m := int(-d / time.Minute)
			if m <= 40320 && len(f.Alerts) < maxAlerts && !containsInt(f.Alerts, m) {
				f.Alerts = append(f.Alerts, m)
			}
		}
	}
	sort.Ints(f.Alerts)
	if len(f.Notes) > maxText {
		f.Notes = f.Notes[:maxText]
	}
	if len(f.Location) > maxText {
		f.Location = f.Location[:maxText]
	}
	if err := f.check(); err != nil {
		return nil, err
	}
	out.f = f
	return out, nil
}

// importInto writes the events of one target calendar, reusing events by UID.
func (a *App) importInto(cs *calState, evs []*importedEvent) (b *batch, added, updated, unchanged int) {
	b = a.newBatch()
	byUID := map[string]*Event{}
	for _, e := range cs.events {
		byUID[uidOf(e)] = e
	}
	now := float64(a.clock.Now().UnixMilli())
	masters := map[string]string{} // uid → event id in this calendar
	// Masters and one-offs first, so overrides find their series.
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].rid == "" && evs[j].rid != "" })
	for _, ie := range evs {
		if ie.uid == "" {
			ie.uid = deriveID("nouid", ie.f.Title, ie.f.Start.String()) + "@import"
		}
		if ie.rid != "" {
			mid := masters[ie.uid]
			if mid == "" {
				if e := byUID[ie.uid]; e != nil {
					mid = e.ID
				}
			}
			if mid == "" {
				continue // an override of a series we do not have
			}
			ie.f.RRule = ""
			var old *Event
			if m := cs.events[mid]; m != nil {
				old = m.overrides[ie.rid]
			}
			if ie.cancelled {
				if m := cs.events[mid]; m == nil || !containsStr(m.Exdates, ie.rid) {
					b.ev(mid, "x."+ie.rid, true)
					updated++
				}
				continue
			}
			if old != nil {
				of := fieldsOf(old)
				n := len(b.ops)
				b.write(old.ID, ie.f, &of)
				if len(b.ops) > n {
					updated++
				} else {
					unchanged++
				}
				continue
			}
			oid := deriveID(mid, ie.rid)
			b.write(oid, ie.f, nil)
			b.ev(oid, "of", mid)
			b.ev(oid, "rid", ie.rid)
			b.ev(oid, "deleted", false)
			b.ev(oid, "created", now)
			added++
			continue
		}
		e := byUID[ie.uid]
		if ie.cancelled {
			if e != nil {
				b.ev(e.ID, "deleted", true)
				updated++
			}
			continue
		}
		if e != nil {
			masters[ie.uid] = e.ID
			of := fieldsOf(e)
			n := len(b.ops)
			b.write(e.ID, ie.f, &of)
			for _, x := range ie.exdates {
				if !containsStr(e.Exdates, x) {
					b.ev(e.ID, "x."+x, true)
				}
			}
			if len(b.ops) > n {
				updated++
			} else {
				unchanged++
			}
			continue
		}
		id := deriveID("ics", cs.id, ie.uid)
		masters[ie.uid] = id
		b.write(id, ie.f, nil)
		if ie.uid != id+"@sol" {
			b.ev(id, "uid", ie.uid)
		}
		b.ev(id, "deleted", false)
		b.ev(id, "created", now)
		for _, x := range ie.exdates {
			b.ev(id, "x."+x, true)
		}
		added++
	}
	return b, added, updated, unchanged
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func (a *App) actImport(ctx context.Context, c sol.Call) (any, error) {
	var in struct{ ICS, Calendar, Name string }
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	if len(in.ICS) > maxICS {
		return nil, bad("file too large (at most %d MB)", maxICS>>20)
	}
	raw, calName, err := parseICS(in.ICS)
	if err != nil {
		return nil, err
	}
	var skipped []string
	groups := map[string][]*importedEvent{}
	for _, ie := range raw {
		ev, err := a.resolve(ie)
		if err != nil {
			if len(skipped) < 20 {
				skipped = append(skipped, oneLine(ie.text("SUMMARY"))+": "+err.Error())
			}
			continue
		}
		key := ""
		if in.Calendar == "" && in.Name == "" {
			key = ev.cal
		}
		groups[key] = append(groups[key], ev)
	}
	if len(groups) == 0 {
		return nil, bad("no events found")
	}
	// Each group goes to a calendar: the one named, else one of mine by name, else a new one.
	type target struct {
		name string
		cs   *calState
		evs  []*importedEvent
	}
	var targets []*target
	for _, key := range sortedKeys(groups) {
		t := &target{evs: groups[key]}
		switch {
		case in.Calendar != "":
			cs, err := a.findCal(in.Calendar)
			if err != nil {
				return nil, err
			}
			if cs.mode == modeRead {
				return nil, readOnly()
			}
			t.cs = cs
		default:
			t.name = key
			if t.name == "" {
				t.name = oneLine(in.Name)
			}
			if t.name == "" {
				t.name = calName
			}
			if t.name == "" {
				t.name = "Imported"
			}
			for _, cs := range a.view() {
				if cs.mode != modeRead && strings.EqualFold(cs.info.Name, t.name) {
					t.cs = cs
					break
				}
			}
		}
		targets = append(targets, t)
	}
	if c.DryRun {
		var into []string
		n := 0
		for _, t := range targets {
			n += len(t.evs)
			if t.cs != nil {
				into = append(into, t.cs.info.Name)
			} else {
				into = append(into, t.name+" (new calendar)")
			}
		}
		return dry(map[string]any{"import": n, "into": into, "skipped": skipped}), nil
	}
	var added, updated, unchanged int
	var names []string
	for _, t := range targets {
		if t.cs == nil {
			id, err := a.createCalendar(ctx, t.name, a.nextColor())
			if err != nil {
				return nil, err
			}
			if t.cs, err = a.findCal(id); err != nil {
				return nil, err
			}
		}
		b, ad, up, un := a.importInto(t.cs, t.evs)
		added, updated, unchanged = added+ad, updated+up, unchanged+un
		names = append(names, t.cs.info.Name)
		if err := a.commit(ctx, t.cs, b); err != nil {
			return nil, err
		}
	}
	if skipped == nil {
		skipped = []string{}
	}
	return map[string]any{"calendars": names, "added": added, "updated": updated, "unchanged": unchanged, "skipped": skipped}, nil
}
