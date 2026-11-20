package daemon

import (
	"slices"
	"strings"
	"time"

	"github.com/apv022/sol/internal/feeds"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/netwatch"
	"github.com/apv022/sol/internal/transport"
)

// Network tunables (package variables so tests can shorten them).
var (
	// NetWatch tunes the network-change and sleep detection (zero: netwatch's defaults).
	NetWatch netwatch.Options
	// EndpointSettle is how long after a network change the endpoint record is checked a
	// second time: transports learn their new addresses (a STUN answer, a DHCP lease) a
	// moment after the change itself.
	EndpointSettle = 5 * time.Second
)

// watchNetwork feeds network changes and wake-ups from sleep to NetworkChanged until the
// daemon closes.
func (d *Daemon) watchNetwork() {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		netwatch.Watch(d.ctx, NetWatch, d.NetworkChanged)
	}()
}

// NetworkChanged tells the daemon the network changed (netwatch, an OS callback, `sol
// network-changed`): the transports re-check themselves (Tor, WireGuard), every session is
// checked and every kept peer redialed now instead of after its backoff, and the endpoint
// record is re-signed and pushed to every peer if this device's addresses moved.
func (d *Daemon) NetworkChanged(reason string) {
	if len(reason) > 64 {
		reason = reason[:64]
	}
	d.log.Info("network changed; checking sessions", "reason", reason)
	for _, t := range d.transports {
		if w, ok := t.(transport.NetworkWatcher); ok {
			w.NetworkChanged(reason)
		}
	}
	d.Sessions.NetworkChanged()
	d.refreshEndpoints()
	d.mu.Lock()
	if d.settle != nil {
		d.settle.Stop()
	}
	if d.ctx.Err() == nil {
		d.settle = time.AfterFunc(EndpointSettle, d.refreshEndpoints)
	}
	d.mu.Unlock()
}

// refreshEndpoints re-signs the endpoint record when the transports' published addresses
// differ from it, and pushes the new bundle to every session.
func (d *Daemon) refreshEndpoints() {
	d.netMu.Lock() // one at a time; Close waits for a running one
	defer d.netMu.Unlock()
	if d.ctx.Err() != nil {
		return
	}
	var eps []identity.Endpoint
	for _, t := range d.transports {
		if !transport.Usable(t) {
			continue
		}
		if a := t.Status().Addresses["node"]; a != "" {
			eps = append(eps, endpointOf(t, a))
		}
	}
	if len(eps) == 0 { // offline for now: keep advertising the last addresses
		return
	}
	if slices.Equal(eps, d.Keyring.Endpoints().Endpoints) {
		return
	}
	if _, err := d.Keyring.SetEndpoints(eps, time.Now()); err != nil {
		d.log.Warn("could not sign the new endpoint record", "err", err)
		return
	}
	d.log.Info("this device's addresses changed; telling its peers")
	d.Sessions.BundleChanged()
}

// watchContacts keeps sessions up with the devices of every contact as the contacts list
// changes, including additions replicated from my other devices: the session manager may
// already know their endpoints from a sibling's relay.
func (d *Daemon) watchContacts() {
	sub := d.Store.Bus().Subscribe(64)
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer sub.Close()
		suffix := "/" + feeds.InternalApp + "/" + feeds.ContactsFeed
		for {
			select {
			case <-d.ctx.Done():
				return
			case ev := <-sub.C():
				if ev.Entry != nil && strings.HasSuffix(ev.Entry.Feed, suffix) {
					d.Sessions.KeepContacts()
					d.kickAccess() // a contact came or went: so do their keys on my onion
				}
			}
		}
	}()
}
