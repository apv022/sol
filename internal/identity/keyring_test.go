package identity

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func mustBundle(t *testing.T, k *Keyring) Bundle {
	t.Helper()
	b, err := k.Bundle()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestKeyringCreateOpenConfirm(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	k, err := Create(dir, "Ada", "laptop", t0)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		for p, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, KeyringFile): 0o600} {
			st, err := os.Stat(p)
			if err != nil || st.Mode().Perm() != want {
				t.Fatalf("%s: mode %v, want %v (%v)", p, st.Mode().Perm(), want, err)
			}
		}
	}
	if _, err := Create(dir, "Ada", "laptop", t0); !errors.Is(err, ErrExists) {
		t.Fatalf("second Create: %v", err)
	}
	if !k.IsAdmin() {
		t.Fatal("first device is not admin")
	}
	b := mustBundle(t, k)
	if _, err := VerifyBundle(b, k.DevicePublic()); err != nil {
		t.Fatal(err)
	}
	words, ok := k.PendingWords()
	if !ok || len(words) != WordCount {
		t.Fatal("no pending words")
	}
	ent, _ := WordsToEntropy(words)
	r, _ := RecoveryKey(ent)
	if !bytes.Equal(pubOf(r), k.Person()) {
		t.Fatal("words do not derive the person ID")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, KeyringFile))
	if !bytes.Contains(raw, ent) {
		t.Fatal("entropy not stored before confirmation")
	}

	k2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mustBundle(t, k2).Marshal(), b.Marshal()) || !k2.IsAdmin() {
		t.Fatal("Open lost state")
	}
	if err := k2.ConfirmBackup(); err != nil {
		t.Fatal(err)
	}
	if _, ok := k2.PendingWords(); ok {
		t.Fatal("words still pending")
	}
	if err := k2.ConfirmBackup(); !errors.Is(err, ErrNoBackup) {
		t.Fatalf("second confirm: %v", err)
	}
	raw, _ = os.ReadFile(filepath.Join(dir, KeyringFile))
	if bytes.Contains(raw, ent) {
		t.Fatal("entropy still on disk after confirmation")
	}
	k3, _ := Open(dir)
	if _, ok := k3.PendingWords(); ok || !k3.IsAdmin() {
		t.Fatal("reopened keyring wrong")
	}
	if _, err := Open(t.TempDir()); !errors.Is(err, ErrNoKeyring) {
		t.Fatalf("Open empty dir: %v", err)
	}
}

func TestKeyringDevices(t *testing.T) {
	admin, err := Create(t.TempDir(), "Ada", "laptop", t0)
	if err != nil {
		t.Fatal(err)
	}
	phone, err := NewDevice(t.TempDir(), t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := phone.Bundle(); !errors.Is(err, ErrNoPerson) {
		t.Fatalf("bundle before join: %v", err)
	}
	cache := NewCache()
	if _, err := cache.Accept(mustBundle(t, admin), admin.DevicePublic()); err != nil {
		t.Fatal(err)
	}

	// Join: admin adds the phone, the phone adopts the list.
	dl, err := admin.AddDevice(phone.DevicePublic(), "phone", FlagAnchor, t0.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	pc := mustBundle(t, admin).Person
	if err := phone.Adopt(pc, dl); err != nil {
		t.Fatal(err)
	}
	if err := phone.Adopt(pc, dl); !errors.Is(err, ErrStale) {
		t.Fatalf("adopting the same list twice: %v", err)
	}
	if phone.IsAdmin() {
		t.Fatal("phone became admin")
	}
	pb := mustBundle(t, phone)
	dev, err := cache.Accept(pb, phone.DevicePublic())
	if err != nil || !dev.Anchor() {
		t.Fatalf("phone bundle: %v %+v", err, dev)
	}
	if _, err := phone.AddDevice(pubOf(key(t)), "x", 0, t0); !errors.Is(err, ErrNotAdmin) {
		t.Fatalf("non-admin AddDevice: %v", err)
	}

	// Remove the phone; it is gone for the cache and can't adopt the list.
	if _, err := admin.RemoveDevice(admin.DevicePublic(), t0); !errors.Is(err, ErrSelf) {
		t.Fatalf("self removal: %v", err)
	}
	dl2, err := admin.RemoveDevice(phone.DevicePublic(), t0.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.RemoveDevice(phone.DevicePublic(), t0); !errors.Is(err, ErrNotInList) {
		t.Fatalf("removing twice: %v", err)
	}
	if err := cache.Update(mustBundle(t, admin), admin.DevicePublic()); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Accept(pb, phone.DevicePublic()); !errors.Is(err, ErrRollback) {
		t.Fatalf("removed phone with its old bundle: %v", err)
	}
	if err := phone.Adopt(pc, dl2); !errors.Is(err, ErrNotInList) {
		t.Fatalf("phone adopting its removal: %v", err)
	}
}

func TestKeyringRotateAndRestore(t *testing.T) {
	a, err := Create(t.TempDir(), "Ada", "laptop", t0)
	if err != nil {
		t.Fatal(err)
	}
	words, _ := a.PendingWords()
	if err := a.ConfirmBackup(); err != nil {
		t.Fatal(err)
	}
	b2, _ := NewDevice(t.TempDir(), t0)
	dl, _ := a.AddDevice(b2.DevicePublic(), "desktop", FlagAdmin, t0.Add(time.Second))
	oldPC := mustBundle(t, a).Person
	if err := b2.Adopt(oldPC, dl); err != nil {
		t.Fatal(err)
	}

	contact := NewCache()
	oldBundle := mustBundle(t, a)
	if _, err := contact.Accept(oldBundle, a.DevicePublic()); err != nil {
		t.Fatal(err)
	}

	// Wrong words are refused.
	other, _ := Create(t.TempDir(), "Bob", "x", t0)
	otherWords, _ := other.PendingWords()
	if err := b2.RotatePerson(otherWords, t0); !errors.Is(err, ErrWrongWords) {
		t.Fatalf("rotate with another identity's words: %v", err)
	}

	// The laptop (admin) is lost: the desktop removes it... it can't, it has
	// no P yet. It rotates P from the words first, then removes the laptop.
	if err := b2.RotatePerson(words, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !b2.IsAdmin() {
		t.Fatal("rotating device does not hold the new P")
	}
	if _, err := b2.RemoveDevice(a.DevicePublic(), t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	rotated := mustBundle(t, b2)
	if bytes.Equal(rotated.Person.Key, oldPC.Key) {
		t.Fatal("P did not change")
	}
	if err := contact.Update(rotated, b2.DevicePublic()); err != nil {
		t.Fatalf("contact refused rotation: %v", err)
	}
	// The old P is useless: the laptop's bundle is a rollback.
	if _, err := contact.Accept(oldBundle, a.DevicePublic()); !errors.Is(err, ErrRollback) {
		t.Fatalf("old P still accepted: %v", err)
	}
	// A list signed by the old P under the new person cert fails the chain.
	forged := DeviceList{Person: rotated.Person.Person, Seq: rotated.Devices.Seq + 1,
		Devices: []Device{{Key: a.DevicePublic(), Flags: FlagAdmin}}}
	_ = forged.Sign(a.st.person)
	if _, err := contact.Accept(Bundle{rotated.Person, forged, oldBundle.Endpoints}, a.DevicePublic()); err == nil {
		t.Fatal("list signed by superseded P accepted")
	}

	// Every device lost: restore from the words on a new device.
	later := t0.Add(24 * time.Hour)
	c, err := Restore(t.TempDir(), words, "Ada", "new phone", later)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.PendingWords(); ok {
		t.Fatal("restore kept the entropy")
	}
	if !bytes.Equal(c.Person(), a.Person()) {
		t.Fatal("restore produced another person ID")
	}
	restored := mustBundle(t, c)
	if len(restored.Devices.Devices) != 1 || !restored.Devices.Devices[0].Admin() {
		t.Fatalf("restored list: %+v", restored.Devices.Devices)
	}
	if _, err := contact.Accept(restored, c.DevicePublic()); err != nil {
		t.Fatalf("contact refused restored identity: %v", err)
	}
	// The desktop's (now superseded) bundle is a rollback for the contact,
	// and when the desktop adopts the restored list it is not in it.
	if _, err := contact.Accept(rotated, b2.DevicePublic()); !errors.Is(err, ErrRollback) {
		t.Fatalf("superseded bundle: %v", err)
	}
	if err := b2.Adopt(restored.Person, restored.Devices); !errors.Is(err, ErrNotInList) {
		t.Fatalf("desktop adopting restored list: %v", err)
	}
	if _, err := Restore(t.TempDir(), words[:23], "Ada", "x", later); !errors.Is(err, ErrBadWords) {
		t.Fatalf("restore with 23 words: %v", err)
	}
}

func TestAdoptDropsStaleP(t *testing.T) {
	a, _ := Create(t.TempDir(), "Ada", "laptop", t0)
	words, _ := a.PendingWords()
	b, _ := NewDevice(t.TempDir(), t0)
	dl, _ := a.AddDevice(b.DevicePublic(), "desktop", 0, t0)
	_ = b.Adopt(mustBundle(t, a).Person, dl)
	if err := b.RotatePerson(words, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	nb := mustBundle(t, b)
	if err := a.Adopt(nb.Person, nb.Devices); err != nil {
		t.Fatal(err)
	}
	if a.IsAdmin() {
		t.Fatal("laptop kept the superseded P")
	}
	if d, _ := nb.Devices.Find(a.DevicePublic()); d.Admin() {
		t.Fatal("laptop still flagged admin after rotation")
	}
	reopened, _ := Open(a.dir)
	if reopened.IsAdmin() {
		t.Fatal("superseded P still on disk")
	}
}

func TestKeyringAgentsAndEndpoints(t *testing.T) {
	k, _ := Create(t.TempDir(), "Ada", "laptop", t0)
	g := pubOf(key(t))
	c, err := k.IssueAgent(g, "helper", []string{"docs.read"}, 0, t0)
	if err != nil {
		t.Fatal(err)
	}
	if c.Expires-c.Issued != uint64(DefaultAgentTTL/time.Second) {
		t.Fatalf("default ttl: %d", c.Expires-c.Issued)
	}
	b := mustBundle(t, k)
	if err := VerifyAgent(c, b, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := k.IssueAgent(g, "helper", []string{"*"}, 91*24*time.Hour, t0); err == nil {
		t.Fatal("91-day agent issued")
	}
	if _, err := k.IssueAgent(g, "helper", nil, 0, t0); err == nil {
		t.Fatal("agent without scopes issued")
	}
	late := t0.Add(29 * 24 * time.Hour)
	r, err := k.RenewAgent(c, 0, late)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAgent(r, b, late.Add(10*24*time.Hour)); err != nil {
		t.Fatalf("renewed cert: %v", err)
	}
	if err := VerifyAgent(c, b, late.Add(10*24*time.Hour)); !errors.Is(err, ErrExpired) {
		t.Fatalf("old cert after expiry: %v", err)
	}
	other, _ := Create(t.TempDir(), "Bob", "x", t0)
	if _, err := other.RenewAgent(c, 0, late); err == nil {
		t.Fatal("renewed by another device")
	}

	cache := NewCache()
	if _, err := cache.Accept(b, k.DevicePublic()); err != nil {
		t.Fatal(err)
	}
	ep, err := k.SetEndpoints([]Endpoint{{"tor", "abc.onion:443", "key"}}, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Update(mustBundle(t, k), k.DevicePublic()); err != nil {
		t.Fatal(err)
	}
	got, ok := cache.Endpoints(k.Person(), k.DevicePublic())
	if !ok || got.Seq != ep.Seq {
		t.Fatal("endpoint update not cached")
	}
	if _, err := cache.Accept(b, k.DevicePublic()); !errors.Is(err, ErrRollback) {
		t.Fatalf("old endpoint record: %v", err)
	}
	sig := k.Signer()
	msg := []byte("transcript")
	s, err := sig.Sign(nil, msg, crypto.Hash(0))
	if err != nil || !ed25519.Verify(k.DevicePublic(), msg, s) {
		t.Fatal("Signer does not sign with the device key")
	}
}

func TestAgentAllows(t *testing.T) {
	c := AgentCert{Scopes: []string{"files.*", "notes.read"}}
	for action, want := range map[string]bool{"files.delete": true, "notes.read": true, "notes.write": false, "filesx.list": false, "music.play": false} {
		if c.Allows(action, "p1") != want {
			t.Errorf("%s: want %v", action, want)
		}
	}
	c = AgentCert{Scopes: []string{"*", "person:p1"}}
	if !c.Allows("x.y", "p1") || c.Allows("x.y", "p2") || !c.AllowsAction("x.y") {
		t.Error("person limits")
	}
}
