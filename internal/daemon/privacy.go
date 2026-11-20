package daemon

// Per-person privacy (design/PROTOCOL.md §Anonymous contacts). My devices and buddies (every
// contact by default) use WireGuard direct first, then TLS direct, Tor last; an anonymous
// contact is Tor only, both ways, and never learns an address of mine. The flag lives in my
// contacts feed, so all my devices agree; each device applies a change it sees (made here or
// replicated) to its own sessions and re-signs its own endpoint record with a higher seq, so
// no contact ever holds both forms (full and onion-only) of one record.

import (
	"crypto/ed25519"
	"time"

	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/sdk/go/sol"
)

// AnonymousWarning is what the Sol app and the CLI say when a contact is made anonymous.
const AnonymousWarning = "From now on they only reach you over Tor and only learn your onion address. " +
	"Any IP address of yours they have already seen can't be taken back."

// SetAnonymous makes a contact anonymous (Tor only, both ways) or, with false, a buddy.
func (d *Daemon) SetAnonymous(id string, anonymous bool) (PersonInfo, error) {
	pub, err := identity.ParseID(id)
	if err != nil || !d.Contacts.IsContact(pub) {
		return PersonInfo{}, sol.Errorf("not_found", "not a contact: "+id)
	}
	if err := d.Contacts.SetAnonymous(pub, anonymous); err != nil {
		return PersonInfo{}, err
	}
	d.privacyChanged()
	d.API.PersonChanged(id)
	p, _ := d.person(id)
	return p, nil
}

// notePrivacy records who is anonymous now, without acting on it (at start).
func (d *Daemon) notePrivacy() {
	d.privMu.Lock()
	defer d.privMu.Unlock()
	d.known, d.anon = map[string]bool{}, map[string]bool{}
	for _, id := range d.Contacts.List() {
		d.known[id] = true
		if pub, err := identity.ParseID(id); err == nil && d.Contacts.Anonymous(pub) {
			d.anon[id] = true
		}
	}
}

// privacyChanged applies every change of a known contact between anonymous and buddy since
// the last call: sessions that no longer fit close, WireGuard learns or forgets them, and
// this device's endpoint record is re-signed and pushed (each peer gets the form it may see).
// A new contact needs none of it: it got the right form when it was paired.
func (d *Daemon) privacyChanged() {
	d.privMu.Lock()
	defer d.privMu.Unlock()
	known, anon := map[string]bool{}, map[string]bool{}
	var changed []ed25519.PublicKey
	for _, id := range d.Contacts.List() {
		pub, err := identity.ParseID(id)
		if err != nil {
			continue
		}
		known[id] = true
		a := d.Contacts.Anonymous(pub)
		if a {
			anon[id] = true
		}
		if d.known[id] && a != d.anon[id] {
			changed = append(changed, pub)
		}
	}
	d.known, d.anon = known, anon
	if len(changed) == 0 || d.ctx.Err() != nil {
		return
	}
	for _, p := range changed {
		d.Sessions.ContactChanged(p)
	}
	d.netMu.Lock()
	_, err := d.Keyring.BumpEndpoints(time.Now())
	d.netMu.Unlock()
	if err != nil {
		d.log.Warn("could not re-sign the endpoint record", "err", err)
		return
	}
	d.log.Info("a contact's privacy changed; re-signed this device's endpoints", "contacts", len(changed))
	d.Sessions.BundleChanged()
}
