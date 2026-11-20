package session

import (
	"crypto/ed25519"
	"sync"

	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/pairing"
	"github.com/apv022/sol/internal/transport"
)

// clientAuth remembers which client-auth key this device installed for which access-restricted
// address (a peer's onion), so a dial installs a key only when it changes and Forget can take
// it back. Keys are secrets: they are never logged.
type clientAuth struct {
	mu   sync.Mutex
	keys map[string]string // address -> installed private key
	of   map[string]string // peer device -> address
}

// authorize gives transport t the credential for ep before it is dialed: the pairing
// secret's key (pairing.AccessKey) for a pair or join, or the key derived for this device on
// device's onion (identity.Keyring.TorAccess) when the endpoint says it needs one.
func (m *Manager) authorize(t transport.Transport, device ed25519.PublicKey, ep identity.Endpoint, secret []byte) {
	ca, ok := t.(transport.ClientAuthorizer)
	if !ok {
		return
	}
	var key identity.X25519Key
	var err error
	switch {
	case len(secret) > 0:
		key, err = pairing.AccessKey(secret)
	case ep.Access == identity.TorAccessV1:
		key, err = m.kr.TorAccess(device, m.kr.DevicePublic())
	default:
		return // an open address
	}
	if err != nil {
		m.log.Debug("no client-auth key for a peer", "peer", short(device), "err", err)
		return
	}
	priv := key.TorPrivate()
	m.auth.mu.Lock()
	if m.auth.keys == nil {
		m.auth.keys, m.auth.of = map[string]string{}, map[string]string{}
	}
	same := m.auth.keys[ep.Address] == priv
	m.auth.keys[ep.Address] = priv
	m.auth.of[string(device)] = ep.Address
	m.auth.mu.Unlock()
	if same {
		return
	}
	if err := ca.AuthorizeClient(ep.Address, priv); err != nil {
		// Remembered by the transport for its next start; the dial says the rest.
		m.log.Debug("installing a client-auth key", "peer", short(device), "transport", t.Name(), "err", err)
	}
}

// deauthorize forgets the key installed for device's onion (it is no longer a peer).
func (m *Manager) deauthorize(device ed25519.PublicKey) {
	m.auth.mu.Lock()
	addr, ok := m.auth.of[string(device)]
	delete(m.auth.of, string(device))
	if ok {
		delete(m.auth.keys, addr)
	}
	m.auth.mu.Unlock()
	if !ok {
		return
	}
	for _, t := range m.cfg.Transports {
		if ca, ok := t.(transport.ClientAuthorizer); ok && t.Handles(addr) {
			_ = ca.DeauthorizeClient(addr)
		}
	}
}
