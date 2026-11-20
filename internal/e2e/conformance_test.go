package e2e

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apv022/sol/internal/apphost"
	sold "github.com/apv022/sol/internal/daemon"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/sdk/go/sol"
	"github.com/apv022/sol/sdk/go/sol/soltest"
)

// startNode initialises and starts a real daemon in a temporary directory, on clearnet over
// loopback, with the given builtins running in process.
func startNode(t *testing.T, o sold.InitOptions, builtins map[string]apphost.Builtin) *sold.Daemon {
	t.Helper()
	dir := t.TempDir()
	o.Config = sold.Config{UI: "127.0.0.1:0", Files: filepath.Join(dir, "SolDrive"),
		Transports: []sold.TransportConfig{{Name: "clearnet", Listen: "127.0.0.1:0"}}}
	o.Builtins = builtins
	if err := sold.Init(dir, o); err != nil {
		t.Fatal(err)
	}
	d, err := sold.Start(sold.Options{Dir: dir, Builtins: builtins, InProcessApps: true,
		Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// joinNode starts a fresh device and joins it to admin's person.
func joinNode(t *testing.T, admin *sold.Daemon, builtins map[string]apphost.Builtin) *sold.Daemon {
	t.Helper()
	d := startNode(t, sold.InitOptions{Join: true}, builtins)
	link, err := admin.Invite(true)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Pair(context.Background(), link); err != nil {
		t.Fatal(err)
	}
	return d
}

// pairNodes makes b a contact of a (b redeems a's pair link).
func pairNodes(t *testing.T, a, b *sold.Daemon) {
	t.Helper()
	link, err := a.Invite(false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Pair(context.Background(), link); err != nil {
		t.Fatal(err)
	}
}

// waitSession waits until a holds a session with b's device.
func waitSession(t *testing.T, a, b *sold.Daemon) {
	t.Helper()
	dev, _ := identity.ParseID(b.DeviceID())
	eventually(t, "a session", func() bool { _, ok := a.Sessions.Session(dev); return ok })
}

// attachDocs serves the soltest docs app next to d and returns the API as that app sees it.
func attachDocs(t *testing.T, d *sold.Daemon) sol.API {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: soltest.Handler()}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	tok, err := d.Host.Attach(soltest.ParsedManifest(), ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c, err := sol.New(d.APIURL(), tok)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// realWorld is the soltest World over four real daemons: Alice's laptop and phone, Bob and
// Carol, all on clearnet over loopback, in this process.
func realWorld(t *testing.T) *soltest.World {
	laptop := startNode(t, sold.InitOptions{Name: "Alice", Device: "laptop"}, nil)
	phone := joinNode(t, laptop, nil)
	bob := startNode(t, sold.InitOptions{Name: "Bob", Device: "desktop"}, nil)
	carol := startNode(t, sold.InitOptions{Name: "Carol", Device: "desktop"}, nil)
	pairNodes(t, laptop, bob)
	pairNodes(t, laptop, carol)
	waitSession(t, laptop, phone)
	waitSession(t, phone, laptop)
	waitSession(t, laptop, bob)
	waitSession(t, bob, laptop)
	waitSession(t, laptop, carol)
	waitSession(t, carol, laptop)
	bp, _ := identity.ParseID(bob.PersonID())
	cp, _ := identity.ParseID(carol.PersonID())
	eventually(t, "the phone to learn the contacts", func() bool {
		return phone.Contacts.IsContact(bp) && phone.Contacts.IsContact(cp)
	})
	byPerson := map[string]*sold.Daemon{laptop.PersonID(): laptop, bob.PersonID(): bob, carol.PersonID(): carol}
	w := &soltest.World{
		A: attachDocs(t, laptop), A2: attachDocs(t, phone), B: attachDocs(t, bob), C: attachDocs(t, carol),
		AliceID: laptop.PersonID(), BobID: bob.PersonID(), CarolID: carol.PersonID(),
		Pinned: func(p, h string) bool { return byPerson[p].Pins.Pinned(strings.TrimPrefix(h, "sha256:")) },
		InboxKinds: func(p string) []string {
			var out []string
			for _, raw := range byPerson[p].API.InboxItems() {
				var it struct{ Kind string }
				_ = json.Unmarshal(raw, &it)
				out = append(out, it.Kind)
			}
			return out
		},
	}
	return w
}

// TestConformance runs the app API conformance suite (sdk/go/sol/soltest, the same scenarios
// memsol passes) through the real sol.Client against real daemons.
func TestConformance(t *testing.T) {
	soltest.Timeout = 20 * time.Second
	soltest.Run(t, realWorld)
}
