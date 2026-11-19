package identity

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/apv022/sol/internal/fsutil"
)

// KeyringFile is the name of the keyring file inside a keyring directory.
const KeyringFile = "identity.key"

const ctxKeyring = "sol keyring v1"

// Keyring errors.
var (
	ErrExists     = errors.New("identity: a keyring already exists in this directory")
	ErrNoKeyring  = errors.New("identity: no keyring in this directory")
	ErrNotAdmin   = errors.New("identity: this device does not hold the person key")
	ErrNoPerson   = errors.New("identity: this device has not joined a person yet")
	ErrWrongWords = errors.New("identity: these words belong to another identity")
	ErrSelf       = errors.New("identity: a device cannot remove itself")
	ErrNoBackup   = errors.New("identity: no unconfirmed recovery words are stored")
)

// Keyring holds the keys of one device, stored in one directory: the device
// key D, the person key P if this is an admin device, the recovery entropy
// until the person confirms the paper backup, and the current person
// certificate, device list and this device's endpoint record.
//
// Everything lives in a single file (KeyringFile, 0600, directory 0700)
// replaced atomically on every change, so a crash during a rotation can
// never leave a new P next to an old certificate. A Keyring is safe for
// concurrent use; it never logs or returns its private keys except through
// Signer, which the handshake and TLS need.
type Keyring struct {
	mu  sync.Mutex
	dir string
	st  state
}

// state is the on-disk content. Methods build a new state, persist it, and
// only then swap it in, so memory never runs ahead of disk.
type state struct {
	device  ed25519.PrivateKey
	person  ed25519.PrivateKey // nil unless admin
	entropy []byte             // nil once the backup is confirmed
	pc      *PersonCert        // nil until the device belongs to a person
	dl      *DeviceList
	ep      EndpointRecord
}

func (s state) marshal() []byte {
	var e enc
	e.str(ctxKeyring)
	e.lp(s.device.Seed())
	var pseed, pc, dl []byte
	if s.person != nil {
		pseed = s.person.Seed()
	}
	if s.pc != nil {
		pc, dl = s.pc.Marshal(), s.dl.Marshal()
	}
	e.lp(pseed)
	e.lp(s.entropy)
	e.lp(pc)
	e.lp(dl)
	e.lp(s.ep.Marshal())
	return e.b
}

func parseState(b []byte) (state, error) {
	d := dec{b: b}
	d.expect(ctxKeyring)
	dseed, pseed, ent, pc, dl, ep := d.lp(), d.lp(), d.lp(), d.lp(), d.lp(), d.lp()
	if err := d.done(); err != nil {
		return state{}, fmt.Errorf("keyring file: %w", err)
	}
	bad := errors.New("identity: keyring file is corrupt")
	if len(dseed) != ed25519.SeedSize || (len(pseed) != 0 && len(pseed) != ed25519.SeedSize) ||
		(len(ent) != 0 && len(ent) != EntropySize) || (len(pc) == 0) != (len(dl) == 0) {
		return state{}, bad
	}
	s := state{device: ed25519.NewKeyFromSeed(dseed)}
	if len(pseed) != 0 {
		s.person = ed25519.NewKeyFromSeed(pseed)
	}
	if len(ent) != 0 {
		s.entropy = ent
	}
	var err error
	if len(pc) != 0 {
		c, err1 := ParsePersonCert(pc)
		l, err2 := ParseDeviceList(dl)
		if err1 != nil || err2 != nil {
			return state{}, bad
		}
		s.pc, s.dl = &c, &l
	}
	if s.ep, err = ParseEndpointRecord(ep); err != nil {
		return state{}, bad
	}
	return s, nil
}

func (k *Keyring) path() string { return filepath.Join(k.dir, KeyringFile) }

// commit persists s and makes it current. Callers hold k.mu.
func (k *Keyring) commit(s state) error {
	if err := fsutil.WriteFileAtomic(k.path(), s.marshal(), 0o600); err != nil {
		return fmt.Errorf("writing keyring: %w", err)
	}
	k.st = s
	return nil
}

// prepare creates dir (0700) and refuses if a keyring is already there.
func prepare(dir string) (*Keyring, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	k := &Keyring{dir: dir}
	if _, err := os.Stat(k.path()); err == nil {
		return nil, ErrExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return k, nil
}

func newKey() (ed25519.PrivateKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	return priv, err
}

// newPerson issues a fresh P, a person certificate signed by r, and a device
// list signed by P holding devs. prevPC and prevDL are the seqs to exceed.
func newPerson(r ed25519.PrivateKey, name string, devs []Device, prevPC, prevDL uint64, now time.Time) (ed25519.PrivateKey, *PersonCert, *DeviceList, error) {
	p, err := newKey()
	if err != nil {
		return nil, nil, nil, err
	}
	rpub := r.Public().(ed25519.PublicKey)
	pc := &PersonCert{Person: rpub, Seq: nextSeq(prevPC, now), Key: p.Public().(ed25519.PublicKey),
		Issued: unix(now), Name: name}
	if err := pc.Sign(r); err != nil {
		return nil, nil, nil, err
	}
	dl := &DeviceList{Person: rpub, Seq: nextSeq(prevDL, now), Devices: devs}
	if err := dl.Sign(p); err != nil {
		return nil, nil, nil, err
	}
	return p, pc, dl, nil
}

func emptyEndpoints(d ed25519.PrivateKey, now time.Time) (EndpointRecord, error) {
	ep := EndpointRecord{Device: d.Public().(ed25519.PublicKey), Seq: nextSeq(0, now)}
	return ep, ep.Sign(d)
}

// Create sets up a new identity on first run: fresh entropy (kept until
// ConfirmBackup), R derived from it, a new P and D, the first person
// certificate, and a device list holding this device as admin.
func Create(dir, personName, deviceName string, now time.Time) (*Keyring, error) {
	entropy := make([]byte, EntropySize)
	if _, err := rand.Read(entropy); err != nil {
		return nil, err
	}
	return create(dir, entropy, true, personName, deviceName, now)
}

// Restore sets up this device from the 24 words after every device was lost:
// a new D, a new P, a person certificate whose seq is above any earlier one
// (it is floored at the current time, see nextSeq), and a new device list
// holding only this device. Contacts accept it because R signed it.
func Restore(dir string, words []string, personName, deviceName string, now time.Time) (*Keyring, error) {
	entropy, err := WordsToEntropy(words)
	if err != nil {
		return nil, err
	}
	return create(dir, entropy, false, personName, deviceName, now)
}

func create(dir string, entropy []byte, keepEntropy bool, personName, deviceName string, now time.Time) (*Keyring, error) {
	if err := checkText(deviceName, "device name", MaxNameLen); err != nil {
		return nil, err
	}
	k, err := prepare(dir)
	if err != nil {
		return nil, err
	}
	r, err := RecoveryKey(entropy)
	if err != nil {
		return nil, err
	}
	d, err := newKey()
	if err != nil {
		return nil, err
	}
	self := Device{Key: d.Public().(ed25519.PublicKey), Name: deviceName, Flags: FlagAdmin, Added: unix(now)}
	p, pc, dl, err := newPerson(r, personName, []Device{self}, 0, 0, now)
	if err != nil {
		return nil, err
	}
	ep, err := emptyEndpoints(d, now)
	if err != nil {
		return nil, err
	}
	s := state{device: d, person: p, pc: pc, dl: dl, ep: ep}
	if keepEntropy {
		s.entropy = entropy
	} else {
		defer clear(entropy)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k, k.commit(s)
}

// NewDevice creates a keyring holding only a device key, for a device that
// will join an existing identity; Adopt then installs the device list an
// admin device signed for it.
func NewDevice(dir string, now time.Time) (*Keyring, error) {
	k, err := prepare(dir)
	if err != nil {
		return nil, err
	}
	d, err := newKey()
	if err != nil {
		return nil, err
	}
	ep, err := emptyEndpoints(d, now)
	if err != nil {
		return nil, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k, k.commit(state{device: d, ep: ep})
}

// Open loads the keyring stored in dir.
func Open(dir string) (*Keyring, error) {
	k := &Keyring{dir: dir}
	b, err := os.ReadFile(k.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoKeyring
	} else if err != nil {
		return nil, err
	}
	if k.st, err = parseState(b); err != nil {
		return nil, err
	}
	return k, nil
}

// DevicePublic returns this device's public key D.
func (k *Keyring) DevicePublic() ed25519.PublicKey {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.st.device.Public().(ed25519.PublicKey)
}

// Signer returns the device key as a crypto.Signer, for handshake
// signatures and the device's TLS certificate.
func (k *Keyring) Signer() crypto.Signer {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.st.device
}

// Person returns the person ID (R's public key), or nil before the device
// has joined a person.
func (k *Keyring) Person() ed25519.PublicKey {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.st.pc == nil {
		return nil
	}
	return k.st.pc.Person
}

// IsAdmin reports whether this device holds the current P.
func (k *Keyring) IsAdmin() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.st.person != nil
}

// Bundle returns this device's identity bundle for handshakes.
func (k *Keyring) Bundle() (Bundle, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.st.pc == nil {
		return Bundle{}, ErrNoPerson
	}
	dl := *k.st.dl
	dl.Devices = append([]Device(nil), dl.Devices...)
	return Bundle{Person: *k.st.pc, Devices: dl, Endpoints: k.st.ep}, nil
}

// PendingWords returns the recovery words while the backup is unconfirmed.
func (k *Keyring) PendingWords() ([]string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.st.entropy == nil {
		return nil, false
	}
	w, err := EntropyToWords(k.st.entropy)
	return w, err == nil
}

// ConfirmBackup records that the person wrote the words down: the entropy is
// removed from disk and memory, leaving only the paper copy.
func (k *Keyring) ConfirmBackup() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.st.entropy == nil {
		return ErrNoBackup
	}
	old := k.st.entropy
	s := k.st
	s.entropy = nil
	if err := k.commit(s); err != nil {
		return err
	}
	clear(old)
	return nil
}

// signList signs a modified copy of the device list with P and commits it.
// Callers hold k.mu.
func (k *Keyring) signList(devs []Device, now time.Time) (DeviceList, error) {
	if k.st.pc == nil {
		return DeviceList{}, ErrNoPerson
	}
	if k.st.person == nil {
		return DeviceList{}, ErrNotAdmin
	}
	dl := &DeviceList{Person: k.st.pc.Person, Seq: nextSeq(k.st.dl.Seq, now), Devices: devs}
	if err := dl.Sign(k.st.person); err != nil {
		return DeviceList{}, err
	}
	s := k.st
	s.dl = dl
	if err := k.commit(s); err != nil {
		return DeviceList{}, err
	}
	return *dl, nil
}

// AddDevice adds (or updates) a device in the device list; admin only. It
// returns the new signed list, to send to the device and push to contacts.
// Setting FlagAdmin only records the role; handing P to that device is the
// caller's job.
func (k *Keyring) AddDevice(pub ed25519.PublicKey, name string, flags uint64, now time.Time) (DeviceList, error) {
	if err := checkKey(pub, "device"); err != nil {
		return DeviceList{}, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.st.dl == nil {
		return DeviceList{}, ErrNoPerson
	}
	entry := Device{Key: append(ed25519.PublicKey(nil), pub...), Name: name, Flags: flags, Added: unix(now)}
	var devs []Device
	for _, d := range k.st.dl.Devices {
		if bytes.Equal(d.Key, pub) {
			entry.Added = d.Added
			continue
		}
		devs = append(devs, d)
	}
	return k.signList(append(devs, entry), now)
}

// RemoveDevice removes a device from the device list; admin only. If it was
// an admin device, also rotate P with RotatePerson, since it held P.
func (k *Keyring) RemoveDevice(pub ed25519.PublicKey, now time.Time) (DeviceList, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.st.dl == nil {
		return DeviceList{}, ErrNoPerson
	}
	if bytes.Equal(pub, k.st.device.Public().(ed25519.PublicKey)) {
		return DeviceList{}, ErrSelf
	}
	var devs []Device
	for _, d := range k.st.dl.Devices {
		if !bytes.Equal(d.Key, pub) {
			devs = append(devs, d)
		}
	}
	if len(devs) == len(k.st.dl.Devices) {
		return DeviceList{}, ErrNotInList
	}
	return k.signList(devs, now)
}

// RotatePerson issues a new P from the words (after an admin device was
// lost): a new person certificate, which makes every older P useless, and the
// current device list re-signed by the new P. This device becomes its only
// holder, so the admin flag is cleared on every other device. Any device of
// the person may do this; the words must match this identity.
func (k *Keyring) RotatePerson(words []string, now time.Time) error {
	entropy, err := WordsToEntropy(words)
	if err != nil {
		return err
	}
	r, err := RecoveryKey(entropy)
	clear(entropy)
	if err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.st.pc == nil {
		return ErrNoPerson
	}
	if !bytes.Equal(r.Public().(ed25519.PublicKey), k.st.pc.Person) {
		return ErrWrongWords
	}
	self := k.st.device.Public().(ed25519.PublicKey)
	devs := make([]Device, 0, len(k.st.dl.Devices))
	for _, d := range k.st.dl.Devices {
		if bytes.Equal(d.Key, self) {
			d.Flags |= FlagAdmin
		} else {
			d.Flags &^= FlagAdmin
		}
		devs = append(devs, d)
	}
	p, pc, dl, err := newPerson(r, k.st.pc.Name, devs, k.st.pc.Seq, k.st.dl.Seq, now)
	if err != nil {
		return err
	}
	s := k.st
	s.person, s.pc, s.dl = p, pc, dl
	return k.commit(s)
}

// Adopt installs a newer person certificate and device list signed elsewhere
// (from an admin device, or the join flow on a NewDevice keyring). It
// verifies the chain, refuses rollbacks, and refuses a list that no longer
// contains this device (ErrNotInList: the device was removed). If the person
// certificate names a P other than the one held here, the stale P is
// dropped.
func (k *Keyring) Adopt(pc PersonCert, dl DeviceList) error {
	if err := pc.Verify(); err != nil {
		return fmt.Errorf("person certificate: %w", err)
	}
	if !bytes.Equal(dl.Person, pc.Person) {
		return fmt.Errorf("device list: %w: belongs to another person", ErrInvalid)
	}
	if err := dl.Verify(pc.Key); err != nil {
		return fmt.Errorf("device list: %w", err)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := dl.Find(k.st.device.Public().(ed25519.PublicKey)); !ok {
		return ErrNotInList
	}
	if k.st.pc != nil {
		if !bytes.Equal(pc.Person, k.st.pc.Person) {
			return invalid("certificate is for another person")
		}
		newer, err := compareIdentity(&cached{pc: *k.st.pc, dl: *k.st.dl}, Bundle{Person: pc, Devices: dl})
		if err != nil {
			return err
		}
		if !newer {
			return ErrStale
		}
	}
	s := k.st
	s.pc, s.dl = &pc, &dl
	if s.person != nil && !bytes.Equal(s.person.Public().(ed25519.PublicKey), pc.Key) {
		s.person = nil
	}
	return k.commit(s)
}

// SetEndpoints signs and stores a new endpoint record for this device.
func (k *Keyring) SetEndpoints(eps []Endpoint, now time.Time) (EndpointRecord, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	ep := EndpointRecord{Device: k.st.device.Public().(ed25519.PublicKey), Seq: nextSeq(k.st.ep.Seq, now),
		Endpoints: append([]Endpoint(nil), eps...)}
	if err := ep.Sign(k.st.device); err != nil {
		return EndpointRecord{}, err
	}
	s := k.st
	s.ep = ep
	if err := k.commit(s); err != nil {
		return EndpointRecord{}, err
	}
	return ep, nil
}

// IssueAgent signs an agent certificate for agent key g with this device's
// key. ttl 0 means DefaultAgentTTL; more than MaxAgentTTL is refused.
func (k *Keyring) IssueAgent(g ed25519.PublicKey, name string, scopes []string, ttl time.Duration, now time.Time) (AgentCert, error) {
	if ttl == 0 {
		ttl = DefaultAgentTTL
	}
	if ttl < time.Second || ttl > MaxAgentTTL {
		return AgentCert{}, invalid("agent lifetime must be between 1s and %v", MaxAgentTTL)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.st.pc == nil {
		return AgentCert{}, ErrNoPerson
	}
	c := AgentCert{Person: k.st.pc.Person, Device: k.st.device.Public().(ed25519.PublicKey),
		Agent: append(ed25519.PublicKey(nil), g...), Name: name, Issued: unix(now),
		Expires: unix(now) + uint64(ttl/time.Second), Scopes: append([]string(nil), scopes...)}
	if err := c.Sign(k.st.device); err != nil {
		return AgentCert{}, err
	}
	return c, nil
}

// RenewAgent reissues c (same agent, name and scopes) from now for ttl. Only
// the device that issued c may renew it.
func (k *Keyring) RenewAgent(c AgentCert, ttl time.Duration, now time.Time) (AgentCert, error) {
	if !bytes.Equal(c.Device, k.DevicePublic()) {
		return AgentCert{}, invalid("agent certificate was issued by another device")
	}
	return k.IssueAgent(c.Agent, c.Name, c.Scopes, ttl, now)
}
