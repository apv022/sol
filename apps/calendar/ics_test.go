package calendar

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/apv022/sol/sdk/go/sol"
)

func TestFoldAndEscape(t *testing.T) {
	long := "DESCRIPTION:" + icsEscape(strings.Repeat("Grüße, from; Zürich\\ ", 8)+"\nline two")
	var sb strings.Builder
	fold(long, &sb)
	for _, l := range strings.Split(strings.TrimSuffix(sb.String(), "\r\n"), "\r\n") {
		if len(l) > 75 {
			t.Errorf("line of %d octets", len(l))
		}
		if !strings.HasPrefix(l, "DESCRIPTION") && !strings.HasPrefix(l, " ") {
			t.Errorf("continuation without a space: %q", l)
		}
	}
	got := unfold(sb.String())
	if len(got) != 1 {
		t.Fatalf("unfold: %d lines", len(got))
	}
	p, _ := parseLine(got[0])
	if want := strings.Repeat("Grüße, from; Zürich\\ ", 8) + "\nline two"; icsUnescape(p.value) != want {
		t.Errorf("round trip:\n got %q\nwant %q", icsUnescape(p.value), want)
	}
	q, ok := parseLine(`ATTENDEE;CN="Doe: John";ROLE=REQ:mailto:j@x`)
	if !ok || q.params["CN"] != "Doe: John" || q.value != "mailto:j@x" {
		t.Errorf("quoted param: %+v", q)
	}
}

const outlook = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nX-WR-CALNAME:Office\r\n" +
	"BEGIN:VTIMEZONE\r\nTZID:W. Europe Standard Time\r\nBEGIN:STANDARD\r\nDTSTART:16010101T030000\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\n" +
	"BEGIN:VEVENT\r\nUID:abc-1\r\nSUMMARY:Planning\\, Q1\r\nDTSTART;TZID=W. Europe Standard Time:20261124T100000\r\nDURATION:PT1H30M\r\n" +
	"DESCRIPTION:Agenda:\\n1. budget\r\n  and more\r\nBEGIN:VALARM\r\nTRIGGER:-PT15M\r\nACTION:DISPLAY\r\nEND:VALARM\r\n" +
	"BEGIN:VALARM\r\nTRIGGER;RELATED=END:PT0S\r\nEND:VALARM\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:abc-2\r\nSUMMARY:Offsite\r\nDTSTART;VALUE=DATE:20261201\r\nDTEND;VALUE=DATE:20261203\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:abc-3\r\nSUMMARY:Call\r\nDTSTART:20261125T150000Z\r\nDTEND:20261125T153000Z\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:abc-4\r\nSUMMARY:Gone\r\nSTATUS:CANCELLED\r\nDTSTART:20261126T150000Z\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:abc-5\r\nSUMMARY:Broken\r\nEND:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

func TestImportForeignICS(t *testing.T) {
	e := newEnv(t)
	a := start(t, e.alice.Devices[0], e.clk, "", false, "Europe/Berlin")
	out := call(t, a, "calendar.import", map[string]any{"ics": outlook})
	if !reflect.DeepEqual(out["calendars"], []any{"Office"}) || out["added"] != 3.0 || len(out["skipped"].([]any)) != 1 {
		t.Fatalf("import: %v", out)
	}
	occ := agenda(t, a, "2026-11-23", "2026-12-05", "Europe/Berlin")
	if got := titles(occ); !reflect.DeepEqual(got, []string{"Planning, Q1", "Call", "Offsite"}) {
		t.Fatalf("titles: %v", got)
	}
	p := occ[0]
	if p["start"] != "2026-11-24T10:00:00+01:00" || p["end"] != "2026-11-24T11:30:00+01:00" || p["tz"] != "Europe/Berlin" ||
		p["notes"] != "Agenda:\n1. budget and more" || !reflect.DeepEqual(p["alerts"], []any{15.0}) {
		t.Errorf("outlook event: %v", p)
	}
	if occ[1]["start"] != "2026-11-25T16:00:00+01:00" || occ[2]["start"] != "2026-12-01" || occ[2]["end"] != "2026-12-03" {
		t.Errorf("utc or all-day: %v / %v", occ[1], occ[2])
	}
	// Importing again changes nothing.
	feed := sol.Address(e.alice.ID, appID, "cal."+occ[0]["calendar"].(string))
	before, _ := a.api.Read(t.Context(), feed, 1, 1000)
	again := call(t, a, "calendar.import", map[string]any{"ics": outlook})
	after, _ := a.api.Read(t.Context(), feed, 1, 1000)
	if again["added"] != 0.0 || again["updated"] != 0.0 || again["unchanged"] != 3.0 || len(after) != len(before) {
		t.Errorf("second import: %v (%d → %d entries)", again, len(before), len(after))
	}
	if _, err := tryCall(a, "calendar.import", map[string]any{"ics": "hello"}); err == nil {
		t.Error("garbage imported")
	}
}

// Export then import into a fresh person's app gives the same events.
func TestICSRoundTrip(t *testing.T) {
	e := newEnv(t)
	a := start(t, e.alice.Devices[0], e.clk, "", false, "America/Los_Angeles")
	b := start(t, e.bob.Devices[0], e.clk, "", false, "Asia/Tokyo")
	work, home := calID(t, a, "Work"), calID(t, a, "Home")
	series := add(t, a, map[string]any{"calendar": work, "title": "Standup", "start": "2026-11-16T09:00", "repeat": "weekdays", "alerts": []int{5, 1440}})
	call(t, a, "calendar.delete", map[string]any{"id": series + "@20261118T170000Z"})
	call(t, a, "calendar.update", map[string]any{"id": series + "@20261119T170000Z", "title": "Standup (demo)", "start": "2026-11-19T10:00", "location": "Room 4"})
	add(t, a, map[string]any{"calendar": work, "title": "Offsite; planning, \"Q1\"", "start": "2026-11-23", "end": "2026-11-25", "notes": "Line one\nLine two \\ done", "color": "purple"})
	add(t, a, map[string]any{"calendar": home, "title": strings.Repeat("Ünïcödé dinner ", 10), "start": "2026-11-20T19:00", "end": "2026-11-20T22:00", "zone": "Europe/Paris", "location": "Café, Paris"})
	add(t, a, map[string]any{"calendar": home, "title": "Birthday", "start": "2026-11-21", "repeat": "FREQ=YEARLY;COUNT=3"})

	ics := call(t, a, "calendar.export", nil)["ics"].(string)
	if !strings.Contains(ics, "RRULE:FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR") || !strings.Contains(ics, "EXDATE:20261118T170000Z") ||
		!strings.Contains(ics, "RECURRENCE-ID:20261119T170000Z") || !strings.Contains(ics, "DTSTART;TZID=America/Los_Angeles:20261116T090000") ||
		!strings.Contains(ics, "DTSTART;VALUE=DATE:20261123") || !strings.Contains(ics, "TRIGGER:-P1D") || !strings.Contains(ics, "X-SOL-CALENDAR:Work") {
		t.Errorf("export is missing parts:\n%s", ics)
	}
	for _, l := range strings.Split(ics, "\r\n") {
		if len(l) > 75 {
			t.Errorf("unfolded line: %q", l)
		}
	}
	imp := call(t, b, "calendar.import", map[string]any{"ics": ics})
	if imp["added"] != 5.0 || len(imp["calendars"].([]any)) != 2 {
		t.Fatalf("import: %v", imp)
	}
	strip := func(os []map[string]any) []map[string]any {
		for _, o := range os {
			for _, k := range []string{"id", "event", "calendar", "mode", "color"} {
				delete(o, k)
			}
		}
		return os
	}
	for _, zone := range []string{"UTC", "Asia/Tokyo"} {
		want := strip(agenda(t, a, "2026-11-15", "2027-11-30", zone))
		got := strip(agenda(t, b, "2026-11-15", "2027-11-30", zone))
		if len(want) < 10 || !reflect.DeepEqual(got, want) {
			t.Errorf("round trip in %s differs:\n got %v\nwant %v", zone, got, want)
		}
	}
	// Into the same person's calendars again: nothing changes.
	again := call(t, a, "calendar.import", map[string]any{"ics": ics})
	if again["added"] != 0.0 || again["updated"] != 0.0 {
		t.Errorf("re-import into the source: %v", again)
	}
	again = call(t, b, "calendar.import", map[string]any{"ics": ics})
	if again["added"] != 0.0 || again["updated"] != 0.0 {
		t.Errorf("re-import into the copy: %v", again)
	}
	// One calendar exports alone, named.
	one := call(t, a, "calendar.export", map[string]any{"calendar": "Home"})
	if one["filename"] != "Home.ics" || !strings.Contains(one["ics"].(string), "X-WR-CALNAME:Home") || strings.Contains(one["ics"].(string), "Standup") {
		t.Errorf("single export: %v", one["filename"])
	}
}

func TestDurationAndZones(t *testing.T) {
	for in, want := range map[string]time.Duration{"PT1H30M": 90 * time.Minute, "-P1D": -24 * time.Hour, "P1W": 7 * 24 * time.Hour, "-PT15M": -15 * time.Minute, "PT0S": 0} {
		if got, err := icsDurationParse(in); err != nil || got != want {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"1H", "PT", "PTXM", "P1H"} {
		if _, err := icsDurationParse(bad); err == nil && bad != "PT" {
			t.Errorf("%s parsed", bad)
		}
	}
	a := &App{loc: time.UTC}
	for in, want := range map[string]string{"Europe/Berlin": "Europe/Berlin", "Eastern Standard Time": "America/New_York", "/mozilla.org/20050126_1/Europe/Paris": "Europe/Paris", "Nowhere": ""} {
		if _, got := a.icsZone(in); got != want {
			t.Errorf("%s → %q, want %q", in, got, want)
		}
	}
}
