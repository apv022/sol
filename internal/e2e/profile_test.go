package e2e

// Profiles (IDENTITY.md §Profile, PROTOCOL.md /p/profile and /p/avatar): my name and picture,
// signed by P, reach my devices and my contacts; an anonymous contact gets the name but never
// the picture; a device without P forwards a change to an admin device.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	sold "github.com/apv022/sol/internal/daemon"
	"github.com/apv022/sol/internal/faultnet"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/session"
	"github.com/apv022/sol/sdk/go/sol"
)

// shownTo is the name and picture URL viewer shows for person.
func shownTo(viewer *sold.Daemon, person string) (name, source, avatar string) {
	for _, p := range viewer.People() {
		if p.ID == person {
			return p.DisplayName, p.NameSource, p.Avatar
		}
	}
	return "", "", ""
}

// profileAt is the profile of person that d holds.
func profileAt(d *sold.Daemon, person string) identity.Profile {
	pk, _ := identity.ParseID(person)
	p, _ := d.Sessions.Profile(pk)
	return p
}

// peerGet does a GET over viewer's session with d's device.
func peerGet(t *testing.T, viewer, d *sold.Daemon, path string) (int, []byte) {
	t.Helper()
	dev, _ := identity.ParseID(d.DeviceID())
	s, ok := viewer.Sessions.Session(dev)
	if !ok {
		t.Fatalf("no session with %s", d.DeviceID())
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://peer"+path, nil)
	resp, err := s.HTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestProfileSync(t *testing.T) {
	old, oldD := session.ProfileTimeout, sold.ProfileTimeout
	session.ProfileTimeout, sold.ProfileTimeout = 5*time.Second, 5*time.Second
	t.Cleanup(func() { session.ProfileTimeout, sold.ProfileTimeout = old, oldD })
	ctx := context.Background()

	fake := faultnet.NewFakeTor(t.TempDir())
	both := []sold.TransportConfig{{Name: "clearnet", Listen: "127.0.0.1:0"}, {Name: "tor"}}
	laptop := startNet(t, netNode{o: sold.InitOptions{Name: "Ada", Device: "laptop"}, tor: fake.Provider(), transports: both})
	phone := startNet(t, netNode{o: sold.InitOptions{Join: true, Device: "phone"}, tor: fake.Provider(), transports: both})
	bob := startNet(t, netNode{o: sold.InitOptions{Name: "Bob", Device: "desktop"}, tor: fake.Provider(), transports: both})
	carol := startNet(t, netNode{o: sold.InitOptions{Name: "Carol", Device: "pc"}, tor: fake.Provider(),
		transports: []sold.TransportConfig{{Name: "tor"}}})
	for _, d := range []*sold.Daemon{laptop, phone, carol} {
		waitOnion(t, d, time.Minute)
	}
	join, err := laptop.Invite(true)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := phone.Pair(ctx, join); err != nil {
		t.Fatal(err)
	}
	waitSession(t, laptop, phone)
	pairNodes(t, laptop, bob)
	link, err := laptop.InviteAnonymous()
	if err != nil {
		t.Fatal(err)
	}
	redeem(t, carol, link)
	me := laptop.PersonID()
	for _, c := range [][2]*sold.Daemon{{bob, laptop}, {bob, phone}, {carol, laptop}, {carol, phone}} {
		waitSession(t, c[0], c[1])
	}
	if mi := phone.Me(); mi.CanSign || mi.Name != "Ada" || mi.NameSource != sold.NameFromCertificate {
		t.Fatalf("phone before any profile: %+v", mi)
	}

	// A rename on the phone (no P): forwarded to the laptop, which signs and pushes it.
	if mi, err := phone.SetName(ctx, "Ada Lovelace"); err != nil || mi.Name != "Ada Lovelace" || mi.NameSource != sold.NameFromProfile {
		t.Fatalf("rename on the phone: %+v %v", mi, err)
	}
	eventually(t, "the new name everywhere", func() bool {
		if laptop.Me().Name != "Ada Lovelace" {
			return false
		}
		for _, v := range []*sold.Daemon{bob, carol} {
			if n, src, _ := shownTo(v, me); n != "Ada Lovelace" || src != sold.NameFromProfile {
				return false
			}
		}
		return true
	})
	// The certificate keeps the first name (signed by R, which no device needs for this).
	if b, _ := laptop.Keyring.Bundle(); b.Person.Name != "Ada" {
		t.Fatalf("certificate name changed: %q", b.Person.Name)
	}

	// A picture set on the laptop: Bob and the phone fetch it.
	pic := append([]byte{0xff, 0xd8, 0xff, 0xe0}, []byte("a tiny jpeg-ish square")...)
	hash := identity.AvatarHash(pic)
	if _, err := laptop.SetAvatar(ctx, []byte("<svg/>")); !errors.Is(err, sol.ErrBadRequest) {
		t.Fatalf("an SVG was accepted: %v", err)
	}
	if mi, err := laptop.SetAvatar(ctx, pic); err != nil || mi.AvatarHash != hash || mi.Avatar == "" {
		t.Fatalf("set avatar: %+v %v", mi, err)
	}
	eventually(t, "the picture at Bob and on the phone", func() bool {
		_, _, av := shownTo(bob, me)
		return av != "" && bob.Sessions.HasAvatar(hash) && phone.Me().Avatar != "" && phone.Sessions.HasAvatar(hash)
	})
	if b, typ, ok := bob.Sessions.Avatar(hash); !ok || typ != "image/jpeg" || string(b) != string(pic) {
		t.Fatalf("Bob's copy: %v %s", ok, typ)
	}

	// Carol (anonymous) has the name at the same seq, never the picture, and /p/avatar refuses her.
	seq := profileAt(laptop, me).Seq
	eventually(t, "Carol has the newest profile", func() bool { return profileAt(carol, me).Seq == seq })
	if p := profileAt(carol, me); p.Avatar != "" || p.Name != "Ada Lovelace" {
		t.Fatalf("Carol's copy: %+v", p)
	}
	if carol.Sessions.HasAvatar(hash) {
		t.Fatal("the picture reached the anonymous contact")
	}
	if _, _, av := shownTo(carol, me); av != "" {
		t.Fatalf("Carol shows a picture: %s", av)
	}
	for _, d := range []*sold.Daemon{laptop, phone} {
		if code, _ := peerGet(t, carol, d, session.AvatarPath+hash); code != http.StatusForbidden {
			t.Fatalf("/p/avatar to the anonymous contact from %s: %d", d.DeviceID(), code)
		}
		code, body := peerGet(t, carol, d, session.ProfilePath)
		if code != http.StatusOK || len(body) < 4 {
			t.Fatalf("/p/profile to Carol: %d", code)
		}
		if p, err := identity.ParseProfile(body[4:]); err != nil || p.Avatar != "" || len(body) != 4+len(p.Marshal()) {
			t.Fatalf("Carol was served %+v %v (%d bytes)", p, err, len(body))
		}
	}
	// Bob may fetch my current picture, not another hash.
	if code, _ := peerGet(t, bob, laptop, session.AvatarPath+hash); code != http.StatusOK {
		t.Fatalf("/p/avatar to a buddy: %d", code)
	}
	if code, _ := peerGet(t, bob, laptop, session.AvatarPath+identity.AvatarHash([]byte("other"))); code != http.StatusForbidden {
		t.Fatalf("/p/avatar of something else: %d", code)
	}

	// Bob's petname for me wins over my profile name.
	if _, err := bob.SetPetname(me, "ada-l"); err != nil {
		t.Fatal(err)
	}
	if n, src, _ := shownTo(bob, me); n != "ada-l" || src != sold.NameFromPetname {
		t.Fatalf("petname: %s %s", n, src)
	}
	if _, err := bob.SetPetname(me, ""); err != nil {
		t.Fatal(err)
	}

	// With the laptop (the only admin device) off, the phone says which device can do it.
	laptopDir := laptop.Dir()
	laptop.Close()
	_, err = phone.SetName(ctx, "Countess")
	var se *sol.Error
	if !errors.As(err, &se) || se.Code != "unavailable" || !strings.Contains(se.Message, "laptop") {
		t.Fatalf("rename with no admin device reachable: %v", err)
	}
	if phone.Me().Name != "Ada Lovelace" {
		t.Fatal("a failed rename changed the phone's name")
	}

	// Restarts keep everything: the laptop's profile and picture, Bob's copies.
	laptop2, err := sold.Start(sold.Options{Dir: laptopDir, InProcessApps: true, TorProvider: fake.Provider(), Log: testLog("laptop2"),
		SameUser: func(_, _ netip.AddrPort) (bool, error) { return true, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { laptop2.Close() })
	if mi := laptop2.Me(); mi.Name != "Ada Lovelace" || mi.AvatarHash != hash || mi.Avatar == "" {
		t.Fatalf("laptop after restart: %+v", mi)
	}
	bobDir := bob.Dir()
	bob.Close()
	bob2, err := sold.Start(sold.Options{Dir: bobDir, InProcessApps: true, TorProvider: fake.Provider(), Log: testLog("bob2"),
		SameUser: func(_, _ netip.AddrPort) (bool, error) { return true, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bob2.Close() })
	if n, _, av := shownTo(bob2, me); n != "Ada Lovelace" || av == "" {
		t.Fatalf("Bob after restart: %q %q", n, av)
	}

	// Removing the picture on the laptop reaches Bob; the phone's rename works again.
	waitSession(t, laptop2, phone)
	if _, err := laptop2.ClearAvatar(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := phone.SetName(ctx, "Countess"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the change at Bob", func() bool {
		n, _, av := shownTo(bob2, me)
		return n == "Countess" && av == ""
	})
}
