package session

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/pairing"
	"github.com/apv022/sol/internal/transport"
	"github.com/apv022/sol/internal/wire"
)

// ErrNothingKnown means the device asked by Recover holds nothing of that person (or refused).
var ErrNothingKnown = errors.New("session: that device knows nothing of this person")

// recoverable is the callee's answer to a recover request for person (PROTOCOL.md "Recover"):
// the newest person certificate and device list I hold of it, as two u32-framed wire forms.
// Only my own person and my contacts are answered; anyone else is refused silently.
func (m *Manager) recoverable(person ed25519.PublicKey) ([]byte, error) {
	var pc identity.PersonCert
	var dl identity.DeviceList
	if me := m.kr.Person(); me != nil && bytes.Equal(me, person) {
		b, err := m.kr.Bundle()
		if err != nil {
			return nil, err
		}
		pc, dl = b.Person, b.Devices
	} else if !m.cfg.Contacts.IsContact(person) {
		return nil, ErrNotRelated
	} else {
		var ok bool
		if pc, dl, ok = m.cache.Latest(person); !ok {
			return nil, ErrNotRelated
		}
	}
	out := binary.BigEndian.AppendUint32(nil, uint32(len(pc.Marshal())))
	out = append(out, pc.Marshal()...)
	out = binary.BigEndian.AppendUint32(out, uint32(len(dl.Marshal())))
	return append(out, dl.Marshal()...), nil
}

// Recover asks the device of a pairing link (one of my surviving devices, or a contact's) for
// the newest person certificate and device list it holds of the person whose recovery key is
// r, over the first of transports that reaches it. It runs before this device has an identity:
// the handshake uses a throwaway device key, and R proves the person. The result is verified
// (R signed the certificate, its P signed the list), so a restore can start its sequence
// numbers above it. secret is the link's pairing secret: it opens the target's onion while
// it is pending (pairing.AccessKey).
func Recover(ctx context.Context, transports []transport.Transport, target identity.EndpointRecord, secret []byte, r ed25519.PrivateKey) (identity.PersonCert, identity.DeviceList, error) {
	var pc identity.PersonCert
	var dl identity.DeviceList
	_, dk, err := ed25519.GenerateKey(nil)
	if err != nil {
		return pc, dl, err
	}
	local := throwaway{dk}
	ep := identity.EndpointRecord{Device: dk.Public().(ed25519.PublicKey), Seq: uint64(time.Now().Unix())}
	if err := ep.Sign(dk); err != nil {
		return pc, dl, err
	}
	var errs []error
	for _, e := range target.Endpoints {
		var t transport.Transport
		for _, x := range transports {
			if x.Name() == e.Transport && x.Handles(e.Address) {
				t = x
				break
			}
		}
		if t == nil {
			continue
		}
		if ca, ok := t.(transport.ClientAuthorizer); ok && len(secret) > 0 {
			if k, err := pairing.AccessKey(secret); err == nil {
				_ = ca.AuthorizeClient(e.Address, k.TorPrivate())
			}
		}
		dctx, cancel := context.WithTimeout(ctx, DialTimeout)
		conn, err := t.Dial(dctx, e.Address)
		cancel()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		pc, dl, err = recoverOver(ctx, conn, local, target.Device, ep.Marshal(), r)
		if err == nil {
			return pc, dl, nil
		}
		errs = append(errs, err)
		if !errors.Is(err, wire.ErrWrongDevice) && !errors.Is(err, wire.ErrTruncated) {
			break
		}
	}
	if len(errs) == 0 {
		return pc, dl, ErrNoRoute
	}
	return pc, dl, errors.Join(errs...)
}

// recoverOver runs one recover handshake on conn and reads the answer.
func recoverOver(ctx context.Context, conn net.Conn, local wire.Local, want ed25519.PublicKey, ep []byte, r ed25519.PrivateKey) (identity.PersonCert, identity.DeviceList, error) {
	var pc identity.PersonCert
	var dl identity.DeviceList
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if _, err := wire.Dial(conn, local, wire.Call{Op: wire.OpRecover, Want: want, Bundle: ep, Recovery: r}); err != nil {
		if errors.Is(err, wire.ErrRejected) {
			return pc, dl, ErrNothingKnown
		}
		return pc, dl, err
	}
	conn.SetReadDeadline(time.Now().Add(wire.HandshakeTimeout))
	read := func() ([]byte, error) {
		var n [4]byte
		if _, err := io.ReadFull(conn, n[:]); err != nil {
			return nil, err
		}
		l := binary.BigEndian.Uint32(n[:])
		if l == 0 || l > maxBundle {
			return nil, fmt.Errorf("session: recovery answer of %d bytes", l)
		}
		b := make([]byte, l)
		_, err := io.ReadFull(conn, b)
		return b, err
	}
	rawPC, err := read()
	if err != nil {
		return pc, dl, err
	}
	rawDL, err := read()
	if err != nil {
		return pc, dl, err
	}
	if pc, err = identity.ParsePersonCert(rawPC); err != nil {
		return pc, dl, err
	}
	if dl, err = identity.ParseDeviceList(rawDL); err != nil {
		return pc, dl, err
	}
	rpub := r.Public().(ed25519.PublicKey)
	if !bytes.Equal(pc.Person, rpub) || !bytes.Equal(dl.Person, rpub) {
		return pc, dl, ErrWrongPeer
	}
	if err := pc.Verify(); err != nil {
		return pc, dl, err
	}
	if err := dl.Verify(pc.Key); err != nil {
		return pc, dl, err
	}
	return pc, dl, nil
}

// throwaway is a wire.Local for a key that exists only for one recover handshake.
type throwaway struct{ k ed25519.PrivateKey }

// DevicePublic implements wire.Local.
func (t throwaway) DevicePublic() ed25519.PublicKey { return t.k.Public().(ed25519.PublicKey) }

// Signer implements wire.Local.
func (t throwaway) Signer() crypto.Signer { return t.k }
