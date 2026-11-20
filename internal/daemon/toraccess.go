package daemon

import (
	"slices"
	"strings"
	"time"

	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/transport"
)

// Tor client authorization (design/PROTOCOL.md §Transports): this device's onion lists one
// client-auth key per device allowed to reach it — every other device of mine, every device
// of every contact, and every pending pair or join secret — and nobody else can even find it.
// The per-device keys are derived from the two device keys (identity.Keyring.TorAccess), so
// no key travels: the endpoint record only says the onion wants them (identity.TorAccessV1).
// The set is recomputed whenever my device list, a contact's device list, the contacts list
// or the pending secrets change, and the onion is re-added (DEL_ONION + ADD_ONION with the
// same service key, so the same address) only when the set really changed. Tor is never sent
// SIGNAL NEWNYM: it would drop the client-auth keys this device installed for its peers.

// issuesAccess reports whether t restricts who reaches a publication (Tor).
func issuesAccess(t transport.Transport) bool {
	_, ok := t.(transport.AccessIssuer)
	return ok && transport.Usable(t)
}

// endpointOf is the endpoint record entry for t's address: one on an access-restricted
// transport says how its keys are made.
func endpointOf(t transport.Transport, addr string) identity.Endpoint {
	e := identity.Endpoint{Transport: t.Name(), Address: addr}
	if issuesAccess(t) {
		e.Access = identity.TorAccessV1
	}
	return e
}

// kickAccess asks watchAccess to recompute the client-auth set. It never blocks.
func (d *Daemon) kickAccess() {
	if d.accessKick == nil {
		return
	}
	select {
	case d.accessKick <- struct{}{}:
	default:
	}
}

// accessSet returns the client-auth public keys my onion lists, sorted, and when the next
// pending secret's key expires. The key this device derives for itself is always there, so
// an onion with nobody else on it is closed, never open.
func (d *Daemon) accessSet() ([]string, time.Time) {
	me := d.Keyring.DevicePublic()
	devs := [][]byte{me}
	if b, err := d.Keyring.Bundle(); err == nil {
		for _, dv := range b.Devices.Devices {
			devs = append(devs, dv.Key)
		}
	}
	if d.Contacts != nil {
		for _, id := range d.Contacts.List() {
			pub, err := identity.ParseID(id)
			if err != nil {
				continue
			}
			if _, dl, ok := d.Cache.Latest(pub); ok {
				for _, dv := range dl.Devices {
					devs = append(devs, dv.Key)
				}
			}
		}
	}
	var keys []string
	for _, dv := range devs {
		if k, err := d.Keyring.TorAccess(me, dv); err == nil {
			keys = append(keys, k.TorPublic())
		}
	}
	var next time.Time
	if d.tokens != nil {
		var pending []string
		pending, next = d.tokens.AccessKeys()
		keys = append(keys, pending...)
	}
	slices.Sort(keys)
	return slices.Compact(keys), next
}

// refreshAccess republishes the node on every access-restricted transport if the client-auth
// set changed since it was last published, and returns when the next pending secret expires.
func (d *Daemon) refreshAccess() time.Time {
	d.accessMu.Lock()
	defer d.accessMu.Unlock()
	keys, next := d.accessSet()
	sig := strings.Join(keys, ",")
	if d.ctx.Err() != nil || d.nodeLn == nil || sig == d.lastAccess {
		return next
	}
	d.lastAccess = sig
	for _, t := range d.transportsNow() {
		if !issuesAccess(t) {
			continue
		}
		p := transport.Publication{ID: "node", Target: d.targetFor(t), ClientAuth: keys}
		if _, err := t.Publish(d.ctx, p); err != nil {
			// A tor that is not up yet keeps the publication and registers it when it is.
			d.log.Debug("republishing the onion with a new client-auth set", "transport", t.Name(), "err", err)
		}
	}
	d.log.Info("updated who may reach this device's onion", "keys", len(keys))
	return next
}

// watchAccess runs refreshAccess on every kick, and when a pending secret expires.
func (d *Daemon) watchAccess() {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		var expiry <-chan time.Time
		for {
			select {
			case <-d.ctx.Done():
				return
			case <-d.accessKick:
			case <-expiry:
			}
			expiry = nil
			if next := d.refreshAccess(); !next.IsZero() {
				expiry = time.After(max(time.Until(next)+time.Second, time.Second))
			}
		}
	}()
}
