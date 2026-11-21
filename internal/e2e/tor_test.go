package e2e

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sold "github.com/apv022/sol/internal/daemon"
	"github.com/apv022/sol/internal/faultnet"
	"github.com/apv022/sol/internal/identity"
	solruntime "github.com/apv022/sol/internal/runtime"
	"github.com/apv022/sol/internal/transport"
)

// startTorNode starts a real daemon whose only transport is Tor, run by p.
func startTorNode(t *testing.T, p solruntime.Provider, o sold.InitOptions) *sold.Daemon {
	t.Helper()
	dir := t.TempDir()
	o.Config = sold.Config{UI: "127.0.0.1:0", Files: filepath.Join(dir, "SolDrive"),
		Transports: []sold.TransportConfig{{Name: "tor"}}}
	if err := sold.Init(dir, o); err != nil {
		t.Fatal(err)
	}
	d, err := sold.Start(sold.Options{Dir: dir, InProcessApps: true, TorProvider: p, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	// Tor starts in the background (it never holds up Start): wait for the onion.
	waitOnion(t, d, 5*time.Minute)
	return d
}

// waitOnion waits until d's endpoint record lists its onion.
func waitOnion(t *testing.T, d *sold.Daemon, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !hasOnion(d) {
		if time.Now().After(deadline) {
			t.Fatal("no onion in the endpoint record")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func hasOnion(d *sold.Daemon) bool {
	for _, e := range d.Keyring.Endpoints().Endpoints {
		if e.Transport == "tor" {
			return true
		}
	}
	return false
}

// redeem redeems link on d, retrying while d's tor is still bootstrapping (a failed dial
// does not burn the secret: only reaching the inviter does).
func redeem(t *testing.T, d *sold.Daemon, link string) {
	t.Helper()
	var err error
	eventually(t, "redeeming the link", func() bool {
		_, _, err = d.Pair(context.Background(), link)
		return err == nil || !strings.Contains(err.Error(), "SOCKS")
	})
	if err != nil {
		t.Fatal(err)
	}
}

// onionOf is d's onion address from its endpoint record, which must say it wants client auth.
func onionOf(t *testing.T, d *sold.Daemon) string {
	t.Helper()
	for _, e := range d.Keyring.Endpoints().Endpoints {
		if e.Transport == "tor" {
			if e.Access != identity.TorAccessV1 {
				t.Fatalf("tor endpoint without client auth: %+v", e)
			}
			return e.Address
		}
	}
	t.Fatal("no onion in the endpoint record")
	return ""
}

// torClient is a bare Tor transport on p, to knock on onions from outside any daemon.
func torClient(t *testing.T, p solruntime.Provider) *transport.Tor {
	t.Helper()
	c := transport.NewTor(t.TempDir(), p)
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Stop() })
	return c
}

// reaches reports whether c gets through to onion.
func reaches(c *transport.Tor, onion string) bool { return knock(c, onion) == nil }

// knock dials onion through c.
func knock(c *transport.Tor, onion string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := c.Dial(ctx, onion)
	if err == nil {
		conn.Close()
	}
	return err
}

// addOnions counts the ADD_ONION commands for onion's service.
func addOnions(fake *faultnet.FakeTor, onion string) int {
	sid := strings.TrimSuffix(onion, ".onion")
	n := 0
	for _, c := range fake.Commands() {
		if f := strings.Fields(c); len(f) >= 3 && f[1] == "ADD_ONION" && f[2] == sid {
			n++
		}
	}
	return n
}

// TestTorClientAuth: a device's onion admits only devices holding a key it issued: a pair
// link's secret opens it for the pairing, then the contact's device-derived key; a stranger
// is refused; unpairing closes it again. The onion is re-added only when the set changes,
// and NEWNYM is never sent.
func TestTorClientAuth(t *testing.T) {
	fake := faultnet.NewFakeTor(t.TempDir())
	p := fake.Provider()
	alice := startTorNode(t, p, sold.InitOptions{Name: "Alice", Device: "laptop"})
	bob := startTorNode(t, p, sold.InitOptions{Name: "Bob", Device: "desktop"})
	aOnion := onionOf(t, alice)
	stranger := torClient(t, p)
	if reaches(stranger, aOnion) {
		t.Fatal("a stranger reached an onion with client auth")
	}

	link, err := alice.Invite(false)
	if err != nil || len(link) > 200 {
		t.Fatalf("%d chars: %v", len(link), err)
	}
	redeem(t, bob, link)
	waitSession(t, alice, bob)
	waitSession(t, bob, alice)

	// Bob's key, as his device derives it, opens Alice's onion; the stranger's still doesn't.
	ap, _ := identity.ParseID(alice.DeviceID())
	bp, _ := identity.ParseID(bob.DeviceID())
	bobKey, err := bob.Keyring.TorAccess(ap, bp)
	if err != nil {
		t.Fatal(err)
	}
	withBob := torClient(t, p)
	if err := withBob.AuthorizeClient(aOnion, bobKey.TorPrivate()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob's key on alice's onion", func() bool { return reaches(withBob, aOnion) })
	if err := knock(stranger, aOnion); err == nil || !strings.Contains(err.Error(), "missing onion client authorization") {
		t.Fatalf("stranger after the pairing: %v", err)
	}

	// A new session after the pairing secret is gone runs on the derived keys (tor is the
	// only transport).
	aliceKey, _ := alice.Keyring.TorAccess(bp, ap)
	withAlice := torClient(t, p)
	_ = withAlice.AuthorizeClient(onionOf(t, bob), aliceKey.TorPrivate())
	eventually(t, "alice's key on bob's onion", func() bool { return reaches(withAlice, onionOf(t, bob)) })
	for _, s := range bob.Sessions.Sessions() {
		s.Close()
	}
	eventually(t, "a redial over tor", func() bool {
		s, ok := bob.Sessions.Session(ap)
		return ok && s.Alive()
	})

	// News that changes nobody's access does not touch the onion.
	settled := addOnions(fake, aOnion)
	name := "tower"
	if _, err := bob.UpdateDevice(bob.DeviceID(), sold.DevicePatch{Name: &name}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if n := addOnions(fake, aOnion); n != settled {
		t.Fatalf("onion re-added %d times for an unchanged set", n-settled)
	}

	// Unpairing takes Bob's key off the onion.
	if err := alice.Unpair(context.Background(), bob.PersonID()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob's key gone", func() bool {
		err := knock(withBob, aOnion)
		return err != nil && strings.Contains(err.Error(), "wrong onion client authorization")
	})
	if addOnions(fake, aOnion) <= settled {
		t.Fatal("the onion was not re-added")
	}
	for _, c := range fake.Commands() {
		if strings.Contains(c, "NEWNYM") {
			t.Fatal("NEWNYM sent:", c)
		}
	}
}

// TestTorClientAuthJoin: a joining device reaches the admin's onion with the join secret, and
// afterwards each of my devices reaches the other's onion with its derived key.
func TestTorClientAuthJoin(t *testing.T) {
	fake := faultnet.NewFakeTor(t.TempDir())
	p := fake.Provider()
	laptop := startTorNode(t, p, sold.InitOptions{Name: "Alice", Device: "laptop"})
	phone := startTorNode(t, p, sold.InitOptions{Join: true, Device: "phone"})
	link, err := laptop.Invite(true)
	if err != nil {
		t.Fatal(err)
	}
	redeem(t, phone, link)
	waitSession(t, laptop, phone)
	lp, _ := identity.ParseID(laptop.DeviceID())
	pp, _ := identity.ParseID(phone.DeviceID())
	pk, _ := phone.Keyring.TorAccess(pp, lp)
	c := torClient(t, p)
	_ = c.AuthorizeClient(onionOf(t, phone), pk.TorPrivate())
	eventually(t, "the laptop's key on the phone's onion", func() bool { return reaches(c, onionOf(t, phone)) })
}

// TestRealTorClientAuth runs an anonymous pairing over the real Tor network (SOL_TEST_TOR=1; it
// takes minutes): client authorization, the onion-only link and a session over Tor. tor comes
// from the daemon's own providers: the bundled one in a -tags bundled build (scripts/fetch-tor.sh
// first), else SOL_TOR_BIN or a tor on PATH.
func TestRealTorClientAuth(t *testing.T) {
	if os.Getenv("SOL_TEST_TOR") != "1" {
		t.Skip("set SOL_TEST_TOR=1 to run against the real Tor network")
	}
	p, err := solruntime.Select(solruntime.TorProviders, "tor", solruntime.Options{BinDir: t.TempDir(),
		Platform: solruntime.PlatformPaths("")})
	if err != nil {
		t.Skip(err)
	}
	t.Logf("tor from the %s provider", p.Name())
	alice := startTorNode(t, p, sold.InitOptions{Name: "Alice", Device: "laptop"})
	bob := startTorNode(t, p, sold.InitOptions{Name: "Bob", Device: "desktop"})
	link, err := alice.InviteAnonymous()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for {
		if _, _, err = bob.Pair(ctx, link); err == nil || ctx.Err() != nil {
			break
		}
		time.Sleep(5 * time.Second) // the descriptor may not be up yet
	}
	if err != nil {
		t.Fatal(err)
	}
	ap, _ := identity.ParseID(alice.DeviceID())
	bobID, _ := identity.ParseID(bob.PersonID())
	if !alice.Contacts.Anonymous(bobID) {
		t.Fatal("the anonymous link made a buddy")
	}
	deadline := time.Now().Add(5 * time.Minute)
	for {
		if s, ok := bob.Sessions.Session(ap); ok && s.Alive() {
			t.Logf("session over %q", s.Path().Transport)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no session over tor")
		}
		time.Sleep(time.Second)
	}
}
