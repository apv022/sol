// Command home-dev serves the Sol app UI with a fake admin API and canned data, so the UI can be
// built and looked at without a daemon. It also serves the kit and sol.js like the daemon does.
// Every mutating call works on in-memory state, and a fake event stream flips a contact's
// presence every few seconds.
//
//	go run ./ui/home/cmd/home-dev            # prints the URL
//	go run ./ui/home/cmd/home-dev -dir ui/home   # serve the UI files from disk (edit and reload)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/sdk/js"
	"github.com/apv022/sol/ui/home"
	"github.com/apv022/sol/ui/kit"
)

type m = map[string]any

type server struct {
	mu      sync.Mutex
	people  []m
	devices []m
	apps    []m
	inbox   []m
	svcs    []m
	tunnels []m
	theme   m
	backup  m
	agents  []m
	subs    map[chan string]bool
	nextID  int
	// My profile (design/ADMIN.md §Profile): a name ("" shows the certificate's) and a picture.
	meName string
	avatar []byte
	avType string
}

// me is GET /admin/v1/me. Callers hold s.mu.
func (s *server) me() m {
	out := m{"id": "alice", "name": "Alice Lindqvist", "name_source": "certificate", "cert_name": "Alice Lindqvist", "can_sign": true}
	if s.meName != "" {
		out["name"], out["name_source"] = s.meName, "profile"
	}
	if s.avatar != nil {
		out["avatar"], out["avatar_hash"] = fmt.Sprintf("/avatars/fake%d", len(s.avatar)), "sha256:fake"
	}
	return out
}

// now and ago give RFC 3339 strings, the one time format of the admin API (design/ADMIN.md).
func now() string { return ago(0) }

func ago(d time.Duration) string { return time.Now().Add(-d).UTC().Format(time.RFC3339) }

func newServer() *server {
	return &server{
		subs: map[chan string]bool{},
		people: []m{
			{"id": "bob", "name": "Bob Okafor", "petname": "Bob", "online": true,
				"devices":        []m{{"id": "bob-phone", "name": "Bob's phone", "online": true, "anchor": false}, {"id": "bob-nas", "name": "Home server", "online": false, "anchor": true}},
				"shares_with_me": []m{{"kind": "app", "name": "Files", "url": "http://files.bob.localhost:7777/"}, {"kind": "service", "name": "jellyfin", "http": true}},
				"i_share":        []m{{"kind": "app", "name": "Notes"}}, "last_seen": now()},
			{"id": "carol", "name": "Carol Reyes", "petname": "Carol", "online": false,
				"devices":        []m{{"id": "carol-laptop", "name": "Carol's laptop", "online": false, "anchor": false}},
				"shares_with_me": []m{}, "i_share": []m{{"kind": "service", "name": "blog"}}, "last_seen": ago(90 * time.Minute)},
		},
		devices: []m{
			{"id": "alice-laptop", "name": "Alice's laptop", "online": true, "admin": true, "anchor": false, "last_seen": now(), "this": true},
			{"id": "alice-phone", "name": "Alice's phone", "online": true, "admin": true, "anchor": false, "last_seen": ago(time.Minute)},
		},
		apps: []m{
			{"id": "files", "name": "Files", "version": "0.4.0", "running": true, "url": "http://files.localhost:7777/", "description": "Your synced folder", "scopes": []string{"files"}},
			{"id": "notes", "name": "Notes", "version": "0.3.1", "running": true, "url": "http://notes.localhost:7777/", "description": "Markdown notes", "scopes": []string{"files"}},
		},
		inbox: []m{
			{"id": "i1", "app": "Files", "kind": "approval", "title": "Bob wants to open Photos 2024", "body": "Read-only access to one folder.", "time": ago(5 * time.Minute)},
			{"id": "i2", "app": "Notes", "kind": "approval", "title": "Carol asked to edit Trip plan", "body": "She can change this note until you remove her.", "time": ago(2 * time.Hour)},
		},
		svcs:    []m{{"name": "blog", "port": 8080, "http": true, "people": []string{"carol"}}},
		tunnels: []m{{"id": "t1", "person": "bob", "service": "jellyfin", "port": 18096, "state": "open", "http": true}},
		theme:   m{"name": "sol", "settings": m{"scheme": "auto", "accent": "", "density": "comfortable", "radius": "rounded", "glass": "on", "font": "system", "wallpaper": "dusk"}},
		backup: m{"words": strings.Fields("orchard velvet anchor candle meadow harbor lantern pepper quiet ribbon silver timber " +
			"unfold violet whisper yellow zephyr acorn bridge copper dolphin ember falcon garden"), "confirmed": false},
		agents: []m{{"name": "claude-laptop", "scopes": []string{"notes:write", "files:read"}, "issued": ago(72 * time.Hour), "expires": time.Now().Add(27 * 24 * time.Hour).UTC().Format(time.RFC3339)}},
	}
}

func (s *server) publish(typ string, data any) {
	b, _ := json.Marshal(data)
	msg := fmt.Sprintf("event: %s\ndata: %s\n\n", typ, b)
	for c := range s.subs {
		select {
		case c <- msg:
		default:
		}
	}
}

func (s *server) find(list []m, key, id string) int {
	for i, x := range list {
		if x[key] == id {
			return i
		}
	}
	return -1
}

func write(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func (s *server) status() m {
	return m{
		"me":            m{"person": m{"id": "alice", "name": s.me()["name"]}, "device": m{"id": "alice-laptop", "name": "Alice's laptop"}, "profile": s.me()},
		"endpoints":     []m{{"addr": "203.0.113.7:7777", "kind": "public"}, {"addr": "192.168.1.20:7777", "kind": "lan"}},
		"transports":    []m{{"name": "clearnet", "state": "up"}, {"name": "relay", "state": "up"}, {"name": "tor", "state": "off"}},
		"peer_sessions": []m{{"id": "s1", "peer": "Bob \u00b7 Bob's phone", "relation": "contact", "since": ago(20 * time.Minute)}},
		"sessions":      []m{{"id": "b1", "client": "Firefox on Linux", "created": ago(24 * time.Hour), "last_used": now(), "expires": ago(-29 * 24 * time.Hour), "current": true}, {"id": "b2", "client": "Chrome on Android", "created": ago(9 * 24 * time.Hour), "last_used": ago(48 * time.Hour), "expires": ago(-21 * 24 * time.Hour)}},
		"storage":       m{"used": 2_400_000_000, "free": 49_800_000_000, "total": 64_000_000_000, "measured": true},
	}
}

func (s *server) admin(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/admin/v1/")
	parts := strings.Split(p, "/")
	if p == "me/avatar" && r.Method == "PUT" {
		b := make([]byte, 64<<10+1)
		n, _ := io.ReadFull(r.Body, b)
		if n == 0 || n > 64<<10 {
			write(w, 400, m{"error": m{"code": "bad_request", "message": "the picture must be at most 64 KiB"}})
			return
		}
		s.avatar, s.avType = b[:n], r.Header.Get("Content-Type")
		s.publish("status", m{})
		write(w, 200, s.me())
		return
	}
	var body m
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	ok := func() { write(w, 200, m{}) }
	switch {
	case p == "status":
		write(w, 200, s.status())
	case p == "me" && r.Method == "PATCH":
		s.meName, _ = body["name"].(string)
		s.publish("status", m{})
		write(w, 200, s.me())
	case p == "me/avatar" && r.Method == "DELETE":
		s.avatar = nil
		s.publish("status", m{})
		write(w, 200, s.me())
	case p == "me":
		write(w, 200, s.me())
	case p == "people":
		write(w, 200, s.people)
	case parts[0] == "people" && len(parts) == 2:
		i := s.find(s.people, "id", parts[1])
		if i < 0 {
			write(w, 404, m{"error": m{"code": "not_found", "message": "No such person"}})
			return
		}
		if r.Method == "PATCH" {
			s.people[i]["petname"] = body["petname"]
		} else {
			s.people = append(s.people[:i], s.people[i+1:]...)
		}
		s.publish("person", m{})
		ok()
	case p == "devices":
		write(w, 200, s.devices)
	case parts[0] == "devices" && len(parts) == 2:
		i := s.find(s.devices, "id", parts[1])
		if i < 0 {
			write(w, 404, m{"error": m{"code": "not_found", "message": "No such device"}})
			return
		}
		if r.Method == "PATCH" {
			for k, v := range body {
				s.devices[i][k] = v
			}
		} else {
			s.devices = append(s.devices[:i], s.devices[i+1:]...)
		}
		s.publish("device", m{})
		ok()
	case p == "invite":
		kind := "pair"
		if j, _ := body["join"].(bool); j {
			kind = "join"
		}
		write(w, 200, m{"link": "sol://" + kind + "?t=2Wq9xKd7mTz4Lr0bVn3HsYfJ8aCe5uPgXo1iQw6RhEkNtMyDcZvB", "expires": ago(-10 * time.Minute)})
	case p == "pair":
		write(w, 200, m{"person": "dave"})
	case p == "apps":
		write(w, 200, s.apps)
	case p == "services" && r.Method == "GET":
		write(w, 200, s.svcs)
	case p == "services":
		s.svcs = append(s.svcs, body)
		s.publish("service", m{})
		ok()
	case parts[0] == "services" && len(parts) == 2:
		i := s.find(s.svcs, "name", parts[1])
		if i >= 0 {
			s.svcs = append(s.svcs[:i], s.svcs[i+1:]...)
		}
		s.publish("service", m{})
		ok()
	case p == "tunnels" && r.Method == "GET":
		write(w, 200, s.tunnels)
	case p == "tunnels":
		s.nextID++
		port := 18100 + s.nextID
		if f, ok := body["port"].(float64); ok && f > 0 {
			port = int(f)
		}
		s.tunnels = append(s.tunnels, m{"id": fmt.Sprint("t", 10+s.nextID), "person": body["person"], "service": body["service"], "port": port, "state": "open", "http": false})
		s.publish("tunnel", m{})
		ok()
	case parts[0] == "tunnels" && len(parts) == 2:
		i := s.find(s.tunnels, "id", parts[1])
		if i >= 0 {
			s.tunnels = append(s.tunnels[:i], s.tunnels[i+1:]...)
		}
		s.publish("tunnel", m{})
		ok()
	case p == "inbox":
		write(w, 200, s.inbox)
	case parts[0] == "inbox" && len(parts) == 3:
		i := s.find(s.inbox, "id", parts[1])
		if i >= 0 {
			s.inbox = append(s.inbox[:i], s.inbox[i+1:]...)
		}
		s.publish("inbox", m{})
		ok()
	case p == "theme" && r.Method == "GET":
		write(w, 200, s.theme)
	case p == "theme":
		s.theme["name"] = body["name"]
		s.publish("theme", s.theme)
		ok()
	case p == "theme/settings":
		st := s.theme["settings"].(m)
		for k, v := range body {
			st[k] = v
		}
		s.publish("theme", s.theme)
		ok()
	case p == "themes":
		write(w, 200, m{"active": s.theme["name"], "settings": s.theme["settings"], "themes": []m{
			{"name": "sol", "title": "Sol", "author": "Sol", "supports": []string{"light", "dark"}, "builtin": true}, {"name": "graphite", "title": "Graphite", "author": "Sol", "supports": []string{"light", "dark"}, "builtin": true},
			{"name": "terminal", "title": "Terminal", "author": "Sol", "supports": []string{"dark"}, "builtin": true}, {"name": "paper", "title": "Paper", "author": "Sol", "supports": []string{"light"}, "builtin": true}}})
	case p == "backup" && r.Method == "GET":
		write(w, 200, s.backup)
	case p == "backup/confirm":
		s.backup = m{"words": nil, "confirmed": true}
		ok()
	case p == "agents":
		write(w, 200, s.agents)
	case p == "wallpapers":
		write(w, 200, []m{})
	case p == "actions":
		write(w, 200, []m{{"id": "files", "name": "Files", "actions": []m{{"name": "files.list", "effect": "read"}, {"name": "files.rename", "effect": "write"}}}})
	case p == "agents/new":
		write(w, 200, m{"agent": m{"name": body["name"], "scopes": body["scopes"]}, "key_file": "{\"name\": \"demo\", \"key\": \"(dev server: not a real key)\"}", "path": "/home/dev/.sol/agents/demo.key"})
	case parts[0] == "agents" && len(parts) == 3:
		i := s.find(s.agents, "name", parts[1])
		if i >= 0 {
			s.agents = append(s.agents[:i], s.agents[i+1:]...)
		}
		ok()
	case p == "logout" || p == "network-changed" || parts[0] == "sessions":
		ok()
	default:
		write(w, 404, m{"error": m{"code": "not_found", "message": "No such endpoint"}})
	}
}

func (s *server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	c := make(chan string, 8)
	s.mu.Lock()
	s.subs[c] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.subs, c); s.mu.Unlock() }()
	fmt.Fprint(w, ": hello\n\n")
	fl.Flush()
	for {
		select {
		case msg := <-c:
			fmt.Fprint(w, msg)
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// flipPresence toggles Carol online/offline every few seconds and announces it.
func (s *server) flipPresence() {
	for range time.Tick(6 * time.Second) {
		s.mu.Lock()
		c := s.people[1]
		on := !c["online"].(bool)
		c["online"] = on
		c["devices"].([]m)[0]["online"] = on
		c["last_seen"] = now()
		s.publish("person", m{"id": "carol"})
		s.mu.Unlock()
	}
}

func main() {
	dir := flag.String("dir", "", "serve the UI files from this directory instead of the embedded copy")
	port := flag.Int("port", 0, "port (default: a free one)")
	flag.Parse()
	s := newServer()
	go s.flipPresence()

	mux := http.NewServeMux()
	mux.HandleFunc("/admin/v1/events", s.events)
	mux.HandleFunc("/admin/v1/", s.admin)
	mux.HandleFunc("/avatars/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		b, t := s.avatar, s.avType
		s.mu.Unlock()
		if b == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", t)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(b)
	})
	mux.Handle("/kit/", http.StripPrefix("/kit/", http.FileServerFS(kit.FS)))
	mux.Handle("/sol.js", http.FileServerFS(js.FS))
	mux.HandleFunc("/_sol/theme.css", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		name, _ := s.theme["name"].(string)
		s.mu.Unlock()
		if _, err := fs.Stat(kit.FS, "themes/"+name+".css"); err != nil {
			name = "sol"
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, kit.FS, "themes/"+name+".css")
	})
	var ui fs.FS = home.FS
	if *dir != "" {
		ui = os.DirFS(*dir)
	}
	// ?shot (for screenshots): hold the page's load event until the data has arrived, and stop animations.
	mux.HandleFunc("/__hold", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1200 * time.Millisecond)
		w.Header().Set("Content-Type", "text/css")
	})
	files := noCache(http.FileServerFS(ui))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" && r.URL.Query().Has("shot") {
			b, err := fs.ReadFile(ui, "index.html")
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			inject := `<style>*{animation:none!important}</style><link rel="stylesheet" href="/__hold">`
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(strings.Replace(string(b), "</head>", inject+"</head>", 1)))
			return
		}
		files.ServeHTTP(w, r)
	})

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Sol app (fake data): http://%s/\n", ln.Addr())
	log.Fatal(http.Serve(ln, mux))
}

func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}
