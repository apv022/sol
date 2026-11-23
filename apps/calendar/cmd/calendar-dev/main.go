// Command calendar-dev runs the Calendar app against an in-memory Sol so a person can try the UI
// in a browser without a daemon: Alice and Bob, paired, each with their own Calendar and UI
// origin, a "Family" calendar Alice shares with Bob for editing, and an invitation from Bob. The
// memsol fake is served at /_sol/ on each UI origin, exactly where the real daemon's bridge is.
//
//	go run ./apps/calendar/cmd/calendar-dev     # Alice http://127.0.0.1:8791/  Bob http://127.0.0.1:8792/
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/apv022/sol/apps/calendar"
	"github.com/apv022/sol/sdk/go/sol"
	"github.com/apv022/sol/sdk/go/sol/memsol"
)

func main() {
	alice := flag.String("addr", "127.0.0.1:8791", "address of Alice's UI")
	bob := flag.String("bob", "127.0.0.1:8792", "address of Bob's UI (empty to skip)")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, *alice, *bob); err != nil {
		log.Fatal(err)
	}
}

type user struct {
	p   *memsol.Person
	app *calendar.App
	c   *memsol.Client
}

func run(ctx context.Context, aliceAddr, bobAddr string) error {
	net0 := memsol.New()
	a, b := net0.AddPerson("Alice"), net0.AddPerson("Bob")
	net0.Pair(a, b)
	m, err := sol.ParseManifest(calendar.ManifestJSON)
	if err != nil {
		return err
	}
	var users []*user
	for _, p := range []*memsol.Person{a, b} {
		c := p.Devices[0].Client("calendar")
		app := calendar.New(c, calendar.Config{Logf: log.Printf})
		p.Install(m, app.Handler())
		if err := app.Start(ctx); err != nil {
			return err
		}
		defer app.Stop()
		users = append(users, &user{p, app, c})
	}
	if err := seed(users[0], users[1]); err != nil {
		return err
	}
	servers := []*http.Server{}
	for i, addr := range []string{aliceAddr, bobAddr} {
		if addr == "" {
			continue
		}
		u := users[i]
		bridge := http.StripPrefix("/_sol", memsol.NewServer(net0))
		tok := u.c.Token()
		front := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/_sol/v1/") {
				r.Header.Set("Authorization", "Bearer "+tok)
				bridge.ServeHTTP(w, r)
				return
			}
			r.Header.Set("Sol-Person", u.p.ID)
			r.Header.Set("Sol-Device", u.p.Devices[0].ID)
			r.Header.Set("Sol-Self", "1")
			u.app.Handler().ServeHTTP(w, r)
		})
		srv := &http.Server{Addr: addr, Handler: front}
		servers = append(servers, srv)
		go func() {
			if err := srv.ListenAndServe(); err != nil && ctx.Err() == nil {
				log.Print(err)
			}
		}()
	}
	fmt.Printf("Calendar dev UI is running.\n  Alice  http://%s/\n", aliceAddr)
	if bobAddr != "" {
		fmt.Printf("  Bob    http://%s/   (edits Family with Alice)\n", bobAddr)
	}
	fmt.Println("Add ?theme=dark to force dark mode. Ctrl-C to stop.")
	<-ctx.Done()
	for _, s := range servers {
		_ = s.Close()
	}
	return nil
}

// do runs an action on a user's app through its handler, as the person's own device.
func (u *user) do(name string, args map[string]any) (map[string]any, error) {
	if args == nil {
		args = map[string]any{}
	}
	body, _ := json.Marshal(args)
	r := httptest.NewRequest("POST", "/_sol/actions/"+name, bytes.NewReader(body))
	r.Header.Set("Sol-Person", u.p.ID)
	r.Header.Set("Sol-Device", u.p.Devices[0].ID)
	r.Header.Set("Sol-Self", "1")
	w := httptest.NewRecorder()
	u.app.Handler().ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 200 {
		return nil, fmt.Errorf("%s: %s", name, w.Body.String())
	}
	return out, nil
}

// at is a wall time n days from today, in the device zone.
func at(n int, hm string) string { return time.Now().AddDate(0, 0, n).Format("2006-01-02") + "T" + hm }
func day(n int) string           { return time.Now().AddDate(0, 0, n).Format("2006-01-02") }

func seed(alice, bob *user) error {
	var first error
	do := func(u *user, name string, args map[string]any) map[string]any {
		out, err := u.do(name, args)
		if err != nil && first == nil {
			first = err
		}
		return out
	}
	cal := func(u *user, name, color string) string {
		out := do(u, "calendar.create_calendar", map[string]any{"name": name, "color": color})
		if out == nil {
			return ""
		}
		return out["calendar"].(map[string]any)["id"].(string)
	}
	add := func(u *user, c string, a map[string]any) string {
		a["calendar"] = c
		out := do(u, "calendar.add", a)
		if out == nil {
			return ""
		}
		return out["event"].(map[string]any)["id"].(string)
	}
	work := cal(alice, "Work", "blue")
	home := cal(alice, "Home", "green")
	fam := cal(alice, "Family", "orange")
	add(alice, work, map[string]any{"title": "Standup", "start": at(-14, "09:30"), "end": at(-14, "09:45"), "repeat": "weekdays", "alerts": []int{5}})
	add(alice, work, map[string]any{"title": "Design review", "start": at(0, "11:00"), "end": at(0, "12:30"), "location": "Room 4", "notes": "Bring the calendar screenshots"})
	add(alice, work, map[string]any{"title": "Lunch with Maya", "start": at(0, "12:00"), "end": at(0, "13:00"), "location": "Café Luna"})
	add(alice, work, map[string]any{"title": "1:1 with Sam", "start": at(1, "15:00"), "end": at(1, "15:30"), "repeat": "weekly"})
	add(alice, work, map[string]any{"title": "Quarterly planning", "start": at(2, "10:00"), "end": at(2, "16:00"), "location": "Main office"})
	add(alice, work, map[string]any{"title": "Conference", "start": day(8), "end": day(11), "location": "Lisbon"})
	add(alice, home, map[string]any{"title": "Gym", "start": at(-6, "18:30"), "end": at(-6, "19:30"), "repeat": "FREQ=WEEKLY;BYDAY=MO,WE,FR"})
	add(alice, home, map[string]any{"title": "Dentist", "start": at(3, "08:30"), "alerts": []int{60}, "location": "Main St 12"})
	add(alice, home, map[string]any{"title": "Pay rent", "start": day(-21), "repeat": "monthly"})
	add(alice, home, map[string]any{"title": "Call with Tokyo office", "start": at(1, "17:00"), "zone": "Asia/Tokyo"})
	add(alice, fam, map[string]any{"title": "Mom's birthday", "start": day(5), "repeat": "yearly", "alerts": []int{1440}})
	add(alice, fam, map[string]any{"title": "Dinner at grandma's", "start": at(4, "19:00"), "end": at(4, "21:30")})
	do(alice, "calendar.share", map[string]any{"calendar": fam, "person": "Bob", "mode": "write"})
	if err := waitFor(func() bool {
		out, err := bob.do("calendar.list_calendars", nil)
		return err == nil && len(out["calendars"].([]any)) == 1
	}); err != nil {
		return fmt.Errorf("bob never saw the shared calendar (%v): %w", first, err)
	}
	add(bob, fam, map[string]any{"title": "Soccer practice", "start": at(-1, "17:00"), "end": at(-1, "18:30"), "repeat": "weekly"})
	bc := cal(bob, "Bob", "purple")
	party := add(bob, bc, map[string]any{"title": "Housewarming", "start": at(6, "19:00"), "end": at(6, "23:00"), "location": "Bob's new place"})
	do(bob, "calendar.invite", map[string]any{"id": party, "people": []string{"Alice"}})
	return first
}

func waitFor(cond func() bool) error {
	for i := 0; i < 300; i++ {
		if cond() {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("timed out")
}
