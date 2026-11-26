package e2e

import (
	"context"
	"log/slog"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/apv022/sol/internal/apphost"
	sold "github.com/apv022/sol/internal/daemon"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/nearby"
)

// nearbyStart starts (or, with a non-empty dir, restarts) a daemon whose nearby beacons travel
// on bus. Clearnet listens on a fresh loopback port each time, so a restarted device's address
// is new and every peer's record of it is stale.
func nearbyStart(t *testing.T, dir string, o sold.InitOptions, bus *nearby.Bus) (*sold.Daemon, *nearby.BusNetwork) {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
		o.Config = sold.Config{UI: "127.0.0.1:0", Files: filepath.Join(dir, "SolDrive"),
			Transports: []sold.TransportConfig{{Name: "clearnet", Listen: "127.0.0.1:0"}}}
		if err := sold.Init(dir, o); err != nil {
			t.Fatal(err)
		}
	}
	bn := bus.Network(netip.MustParseAddr("127.0.0.1"))
	d, err := sold.Start(sold.Options{Dir: dir, Builtins: map[string]apphost.Builtin{}, InProcessApps: true,
		Log: slog.New(slog.DiscardHandler), NearbyNet: bn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d, bn
}

func connected(a, b *sold.Daemon) bool {
	dev, _ := identity.ParseID(b.DeviceID())
	_, ok := a.Sessions.Session(dev)
	return ok
}

// TestNearbyFindsStaleDevices: Alice's laptop and phone and her buddy Bob all restart on new
// addresses, so every record they hold of each other is stale and nothing could tell them the
// new ones. Their beacons do: every pair reconnects over the address a beacon came from. With
// nearby off they stay apart and send no beacon at all; switched back on, they find each
// other again.
func TestNearbyFindsStaleDevices(t *testing.T) {
	bus := nearby.NewBus()
	laptop, _ := nearbyStart(t, "", sold.InitOptions{Name: "Alice", Device: "laptop"}, bus)
	phone, _ := nearbyStart(t, "", sold.InitOptions{Join: true}, bus)
	link, err := laptop.Invite(true)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := phone.Pair(context.Background(), link); err != nil {
		t.Fatal(err)
	}
	bob, _ := nearbyStart(t, "", sold.InitOptions{Name: "Bob", Device: "desktop"}, bus)
	pairNodes(t, laptop, bob)
	all := func(ds ...*sold.Daemon) func() bool {
		return func() bool {
			for _, a := range ds {
				for _, b := range ds {
					if a != b && !connected(a, b) {
						return false
					}
				}
			}
			return true
		}
	}
	eventually(t, "everyone connected", all(laptop, phone, bob))

	restart := func(ds ...*sold.Daemon) ([]*sold.Daemon, []*nearby.BusNetwork) {
		dirs := make([]string, len(ds))
		for i, d := range ds {
			dirs[i] = d.Dir()
			d.Close()
		}
		var out []*sold.Daemon
		var nets []*nearby.BusNetwork
		for _, dir := range dirs {
			d, n := nearbyStart(t, dir, sold.InitOptions{}, bus)
			out, nets = append(out, d), append(nets, n)
		}
		return out, nets
	}
	ds, _ := restart(laptop, phone, bob)
	eventually(t, "everyone reconnected over beacons", all(ds...))
	for _, a := range ds {
		for _, s := range a.Sessions.Sessions() {
			if p := s.Path(); !p.Inbound && p.Transport != "clearnet" {
				t.Fatalf("a session over %+v", p)
			}
		}
	}

	// Off on every device, then restart on new addresses again: nothing is sent, nobody meets.
	for _, d := range ds {
		if st, err := d.SetNearby(false); err != nil || st.Enabled || st.State != "off" {
			t.Fatalf("SetNearby(false): %+v %v", st, err)
		}
	}
	ds, nets := restart(ds...)
	if st := ds[0].Nearby(); st.Enabled {
		t.Fatalf("nearby came back on after a restart: %+v", st)
	}
	time.Sleep(2 * time.Second)
	for i, n := range nets {
		if n.Sent() != 0 {
			t.Fatalf("device %d sent %d beacons while nearby was off", i, n.Sent())
		}
	}
	if connected(ds[0], ds[1]) || connected(ds[0], ds[2]) || connected(ds[1], ds[2]) {
		t.Fatal("stale devices met without beacons")
	}
	row := ds[0].Transports()[len(ds[0].Transports())-1]
	if row.Name != "nearby" || row.Enabled {
		t.Fatalf("transports row: %+v", row)
	}

	// On again (the transports switch works too): they find each other.
	for _, d := range ds {
		if _, err := d.SetTransport("nearby", true); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "everyone reconnected after nearby came back on", all(ds...))
	if st := ds[0].Nearby(); st.State != "up" || st.Seen < 1 {
		t.Fatalf("status: %+v", st)
	}
}
