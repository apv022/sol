// Command tasks-dev runs the Tasks app against an in-memory Sol so a person can try the UI in a
// browser without a daemon: Alice and Bob, paired, each with their own Tasks and UI origin, and a
// shared "Groceries" list they both edit. The memsol fake is served at /_sol/ on each UI origin,
// exactly where the real daemon's bridge is.
//
//	go run ./apps/tasks/cmd/tasks-dev     # Alice http://127.0.0.1:8789/  Bob http://127.0.0.1:8790/
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

	"github.com/apv022/sol/apps/tasks"
	"github.com/apv022/sol/sdk/go/sol"
	"github.com/apv022/sol/sdk/go/sol/memsol"
)

func main() {
	alice := flag.String("addr", "127.0.0.1:8789", "address of Alice's UI")
	bob := flag.String("bob", "127.0.0.1:8790", "address of Bob's UI (empty to skip)")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, *alice, *bob); err != nil {
		log.Fatal(err)
	}
}

type user struct {
	p   *memsol.Person
	app *tasks.App
	c   *memsol.Client
}

func run(ctx context.Context, aliceAddr, bobAddr string) error {
	net0 := memsol.New()
	a, b := net0.AddPerson("Alice"), net0.AddPerson("Bob")
	net0.Pair(a, b)
	m, err := sol.ParseManifest(tasks.ManifestJSON)
	if err != nil {
		return err
	}
	var users []*user
	for _, p := range []*memsol.Person{a, b} {
		c := p.Devices[0].Client("tasks")
		app := tasks.New(c, tasks.Config{Logf: log.Printf})
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
	fmt.Printf("Tasks dev UI is running.\n  Alice  http://%s/\n", aliceAddr)
	if bobAddr != "" {
		fmt.Printf("  Bob    http://%s/   (shares Groceries with Alice)\n", bobAddr)
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

func day(n int) string { return time.Now().AddDate(0, 0, n).Format("2006-01-02") }

func seed(alice, bob *user) error {
	var first error
	do := func(u *user, name string, args map[string]any) map[string]any {
		out, err := u.do(name, args)
		if err != nil && first == nil {
			first = err
		}
		return out
	}
	list := func(u *user, name, color, icon string) string {
		out := do(u, "tasks.create_list", map[string]any{"name": name, "color": color, "icon": icon})
		if out == nil {
			return ""
		}
		return out["list"].(map[string]any)["id"].(string)
	}
	add := func(u *user, l string, a map[string]any) string {
		a["list"] = l
		out := do(u, "tasks.add", a)
		if out == nil {
			return ""
		}
		return out["task"].(map[string]any)["id"].(string)
	}
	home := list(alice, "Reminders", "blue", "list")
	groc := list(alice, "Groceries", "green", "tag")
	work := list(alice, "Work", "orange", "file")
	trip := list(alice, "Lisbon trip", "purple", "globe")
	add(alice, home, map[string]any{"title": "Call the dentist", "due": day(-1), "time": "09:30", "notes": "Ask about the new appointment slots"})
	add(alice, home, map[string]any{"title": "Water the plants", "due": day(0), "repeat": "weekly"})
	add(alice, home, map[string]any{"title": "Pay rent", "due": day(0), "time": "18:00", "flag": true, "repeat": "monthly"})
	add(alice, home, map[string]any{"title": "Renew passport", "due": day(9), "notes": "Photos in the drawer"})
	add(alice, home, map[string]any{"title": "Book a haircut"})
	done := add(alice, home, map[string]any{"title": "Order a birthday card", "due": day(-2)})
	do(alice, "tasks.complete", map[string]any{"id": done})
	add(alice, work, map[string]any{"title": "Send the Q4 plan to Maya", "due": day(1), "time": "10:00", "flag": true})
	rev := add(alice, work, map[string]any{"title": "Review pull requests", "due": day(0), "time": "16:00"})
	for _, s := range []string{"Sol sync fix", "Notes export", "Tasks UI"} {
		do(alice, "tasks.add", map[string]any{"title": s, "parent": rev})
	}
	add(alice, work, map[string]any{"title": "Team retro", "due": day(3), "time": "14:00", "repeat": "weekly"})
	add(alice, work, map[string]any{"title": "Expense report", "due": day(6)})
	add(alice, trip, map[string]any{"title": "Book flights", "due": day(2), "flag": true})
	add(alice, trip, map[string]any{"title": "Reserve the apartment", "due": day(5)})
	add(alice, trip, map[string]any{"title": "Download offline maps"})
	// The shared list: Alice owns it, Bob may edit.
	for _, t := range []string{"Oat milk", "Sourdough", "Tomatoes", "Basil"} {
		add(alice, groc, map[string]any{"title": t})
	}
	eggs := add(alice, groc, map[string]any{"title": "Eggs (a dozen)", "due": day(0)})
	do(alice, "tasks.share", map[string]any{"list": groc, "person": "Bob", "mode": "write"})
	if err := waitFor(func() bool {
		out, err := bob.do("tasks.list_lists", nil)
		return err == nil && len(out["lists"].([]any)) == 1
	}); err != nil {
		return fmt.Errorf("bob never saw the shared list (%v): %w", first, err)
	}
	do(bob, "tasks.add", map[string]any{"list": groc, "title": "Olive oil", "flag": true})
	do(bob, "tasks.add", map[string]any{"list": groc, "title": "Parmesan"})
	do(bob, "tasks.complete", map[string]any{"id": eggs})
	home2 := list(bob, "Bob's list", "teal", "home")
	add(bob, home2, map[string]any{"title": "Fix the bike light", "due": day(1)})
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
