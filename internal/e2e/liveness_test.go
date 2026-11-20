package e2e

import (
	"log/slog"
	"testing"

	sold "github.com/apv022/sol/internal/daemon"
	"github.com/apv022/sol/internal/identity"
)

// TestRestartKeepsIdentityCache: what a daemon learned of a contact's devices by push (a
// device joined, then was removed) survives its restart, even with nobody online to ask;
// the removed device is not redialed. A network change that moves no address re-signs
// nothing, and every session reports its path.
func TestRestartKeepsIdentityCache(t *testing.T) {
	laptop := startNode(t, sold.InitOptions{Name: "Alice", Device: "laptop"}, nil)
	bob := startNode(t, sold.InitOptions{Name: "Bob", Device: "desktop"}, nil)
	pairNodes(t, laptop, bob)
	waitSession(t, bob, laptop)
	phone := joinNode(t, laptop, nil)
	alice, _ := identity.ParseID(laptop.PersonID())
	phoneKey, _ := identity.ParseID(phone.DeviceID())
	listed := func(d *sold.Daemon) (uint64, bool) {
		_, dl, ok := d.Cache.Latest(alice)
		if !ok {
			return 0, false
		}
		_, in := dl.Find(phoneKey)
		return dl.Seq, in
	}
	eventually(t, "bob learns of the phone by push", func() bool { _, in := listed(bob); return in })
	waitSession(t, bob, phone) // the phone learned of Bob through the laptop's relay
	for _, s := range bob.Status().PeerSessions {
		if s.Path == nil || (s.Path.Transport == "" && !s.Path.Inbound) || s.Path.Reason == "" {
			t.Fatalf("session without a path: %+v", s)
		}
	}

	if err := laptop.RemoveDevice(phone.DeviceID()); err != nil {
		t.Fatal(err)
	}
	b, _ := laptop.Keyring.Bundle()
	eventually(t, "bob learns of the removal by push", func() bool { seq, in := listed(bob); return !in && seq == b.Devices.Seq })

	seq := laptop.Keyring.Endpoints().Seq
	laptop.NetworkChanged("test")
	if laptop.Keyring.Endpoints().Seq != seq {
		t.Fatal("a network change that moved nothing re-signed the endpoint record")
	}

	dir := bob.Dir()
	bob.Close()
	laptop.Close()
	phone.Close()
	bob2, err := sold.Start(sold.Options{Dir: dir, InProcessApps: true, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	defer bob2.Close()
	if seq, in := listed(bob2); in || seq != b.Devices.Seq {
		t.Fatalf("after the restart: list seq %d (want %d), phone listed %v", seq, b.Devices.Seq, in)
	}
}
