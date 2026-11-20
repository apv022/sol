package identity

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Context strings: the first field of every signed object (IDENTITY.md).
const (
	ctxPersonCert = "sol person-cert v1"
	ctxDeviceList = "sol device-list v1"
	ctxAgentCert  = "sol agent-cert v1"
	ctxEndpoints  = "sol endpoints v1"
)

// Device list flags.
const (
	FlagAdmin  uint64 = 1 << 0 // the device holds P
	FlagAnchor uint64 = 1 << 1 // the device is always on
)

// Limits that keep signed objects small and decoding cheap.
const (
	MaxNameLen   = 256  // bytes, for person, device and agent names
	MaxDevices   = 256  // entries in a device list
	MaxEndpoints = 64   // entries in an endpoint record
	MaxScopes    = 64   // scopes in an agent certificate
	maxValueLen  = 4096 // bytes, for scopes and endpoint fields
)

// Agent certificate lifetimes.
const (
	MaxAgentTTL     = 90 * 24 * time.Hour
	DefaultAgentTTL = 30 * 24 * time.Hour
)

// clockSkew is how far in the future an agent certificate's issue time may
// be before it is refused.
const clockSkew = 5 * time.Minute

// Errors from verification. Wrapped errors may add detail, never key bytes.
var (
	ErrBadSignature = errors.New("identity: bad signature")
	ErrInvalid      = errors.New("identity: invalid object")
	ErrExpired      = errors.New("identity: agent certificate expired or not yet valid")
	ErrNotInList    = errors.New("identity: device is not in the device list")
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

func checkKey(k ed25519.PublicKey, what string) error {
	if len(k) != ed25519.PublicKeySize {
		return invalid("%s key must be %d bytes", what, ed25519.PublicKeySize)
	}
	return nil
}

func checkText(s, what string, max int) error {
	if len(s) > max || !utf8.ValidString(s) {
		return invalid("%s must be valid UTF-8 of at most %d bytes", what, max)
	}
	return nil
}

// sign signs body with priv after checking priv belongs to want.
func sign(priv ed25519.PrivateKey, want ed25519.PublicKey, body []byte) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, invalid("private key must be %d bytes", ed25519.PrivateKeySize)
	}
	if !bytes.Equal(priv.Public().(ed25519.PublicKey), want) {
		return nil, invalid("signing key does not match the signer named in the object")
	}
	return ed25519.Sign(priv, body), nil
}

func verifySig(pub ed25519.PublicKey, body, sig []byte) error {
	if len(pub) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize || !ed25519.Verify(pub, body, sig) {
		return ErrBadSignature
	}
	return nil
}

// marshalSigned is the wire form of every signed object: lp(body) | lp(sig).
func marshalSigned(body, sig []byte) []byte {
	var e enc
	e.lp(body)
	e.lp(sig)
	return e.b
}

// unmarshalSigned splits the wire form back into body and signature.
func unmarshalSigned(b []byte) (body, sig []byte, err error) {
	d := dec{b: b}
	body, sig = d.lp(), d.lp()
	return body, sig, d.done()
}

// keyField reads an lp field that must be an ed25519 public key.
func keyField(d *dec) ed25519.PublicKey {
	k := d.lp()
	if d.err == nil && len(k) != ed25519.PublicKeySize {
		d.fail()
	}
	return ed25519.PublicKey(k)
}

// PersonCert names the current person key P of the person whose ID is R's
// public key. Signed by R. The highest Seq wins.
type PersonCert struct {
	Person ed25519.PublicKey // R_pub: the person ID
	Seq    uint64
	Key    ed25519.PublicKey // P_pub
	Issued uint64            // unix seconds
	Name   string
	Sig    []byte
}

// Body returns the signed bytes.
func (c PersonCert) Body() []byte {
	var e enc
	e.str(ctxPersonCert)
	e.lp(c.Person)
	e.u64(c.Seq)
	e.lp(c.Key)
	e.u64(c.Issued)
	e.str(c.Name)
	return e.b
}

func (c PersonCert) check() error {
	if err := checkKey(c.Person, "person"); err != nil {
		return err
	}
	if err := checkKey(c.Key, "person-cert P"); err != nil {
		return err
	}
	return checkText(c.Name, "name", MaxNameLen)
}

// Sign fills Sig using r, which must be the key named in c.Person.
func (c *PersonCert) Sign(r ed25519.PrivateKey) error {
	if err := c.check(); err != nil {
		return err
	}
	sig, err := sign(r, c.Person, c.Body())
	c.Sig = sig
	return err
}

// Verify checks the certificate is well formed and signed by c.Person.
func (c PersonCert) Verify() error {
	if err := c.check(); err != nil {
		return err
	}
	return verifySig(c.Person, c.Body(), c.Sig)
}

// Marshal returns the wire form: lp(body) | lp(sig).
func (c PersonCert) Marshal() []byte { return marshalSigned(c.Body(), c.Sig) }

// ParsePersonCert decodes a wire form. It does not verify the signature.
func ParsePersonCert(b []byte) (PersonCert, error) {
	body, sig, err := unmarshalSigned(b)
	if err != nil {
		return PersonCert{}, err
	}
	d := dec{b: body}
	d.expect(ctxPersonCert)
	c := PersonCert{Person: keyField(&d), Seq: d.u64(), Key: keyField(&d), Issued: d.u64(), Name: d.str(), Sig: sig}
	return c, d.done()
}

// Device is one entry of a device list.
type Device struct {
	Key   ed25519.PublicKey // D_pub
	Name  string
	Flags uint64 // FlagAdmin, FlagAnchor; unknown bits are kept and ignored
	Added uint64 // unix seconds
}

// Admin reports whether the device holds P.
func (d Device) Admin() bool { return d.Flags&FlagAdmin != 0 }

// Anchor reports whether the device is always on.
func (d Device) Anchor() bool { return d.Flags&FlagAnchor != 0 }

// DeviceList is the set of devices that belong to a person. Signed by P.
// The highest Seq wins; a device not in the latest list is not the person's.
type DeviceList struct {
	Person  ed25519.PublicKey // R_pub
	Seq     uint64
	Devices []Device
	Sig     []byte
}

// Body returns the signed bytes.
func (l DeviceList) Body() []byte {
	var e enc
	e.str(ctxDeviceList)
	e.lp(l.Person)
	e.u64(l.Seq)
	e.count(len(l.Devices))
	for _, d := range l.Devices {
		e.lp(d.Key)
		e.str(d.Name)
		e.u64(d.Flags)
		e.u64(d.Added)
	}
	return e.b
}

func (l DeviceList) check() error {
	if err := checkKey(l.Person, "person"); err != nil {
		return err
	}
	if len(l.Devices) > MaxDevices {
		return invalid("more than %d devices", MaxDevices)
	}
	seen := make(map[string]bool, len(l.Devices))
	for _, d := range l.Devices {
		if err := checkKey(d.Key, "device"); err != nil {
			return err
		}
		if err := checkText(d.Name, "device name", MaxNameLen); err != nil {
			return err
		}
		if seen[string(d.Key)] {
			return invalid("device listed twice")
		}
		seen[string(d.Key)] = true
	}
	return nil
}

// Sign fills Sig using p, the current person key.
func (l *DeviceList) Sign(p ed25519.PrivateKey) error {
	if err := l.check(); err != nil {
		return err
	}
	if len(p) != ed25519.PrivateKeySize {
		return invalid("private key must be %d bytes", ed25519.PrivateKeySize)
	}
	l.Sig = ed25519.Sign(p, l.Body())
	return nil
}

// Verify checks the list is well formed and signed by the person key p
// (taken from a verified PersonCert for the same person).
func (l DeviceList) Verify(p ed25519.PublicKey) error {
	if err := l.check(); err != nil {
		return err
	}
	return verifySig(p, l.Body(), l.Sig)
}

// Find returns the entry for device key d.
func (l DeviceList) Find(d ed25519.PublicKey) (Device, bool) {
	for _, x := range l.Devices {
		if bytes.Equal(x.Key, d) {
			return x, true
		}
	}
	return Device{}, false
}

// Marshal returns the wire form: lp(body) | lp(sig).
func (l DeviceList) Marshal() []byte { return marshalSigned(l.Body(), l.Sig) }

// ParseDeviceList decodes a wire form. It does not verify the signature.
func ParseDeviceList(b []byte) (DeviceList, error) {
	body, sig, err := unmarshalSigned(b)
	if err != nil {
		return DeviceList{}, err
	}
	d := dec{b: body}
	d.expect(ctxDeviceList)
	l := DeviceList{Person: keyField(&d), Seq: d.u64(), Sig: sig}
	n := d.count(MaxDevices)
	for i := 0; i < n && d.err == nil; i++ {
		l.Devices = append(l.Devices, Device{Key: keyField(&d), Name: d.str(), Flags: d.u64(), Added: d.u64()})
	}
	return l, d.done()
}

// AgentCert authorizes agent key G to act for the person through device D,
// within Scopes, until Expires. Signed by D.
type AgentCert struct {
	Person  ed25519.PublicKey // R_pub
	Device  ed25519.PublicKey // D_pub, the signer
	Agent   ed25519.PublicKey // G_pub
	Name    string
	Issued  uint64   // unix seconds
	Expires uint64   // unix seconds, at most MaxAgentTTL after Issued
	Scopes  []string // action patterns ("files.*", "docs.read", "*") and "person:<id>" limits
	Sig     []byte
}

// Body returns the signed bytes.
func (c AgentCert) Body() []byte {
	var e enc
	e.str(ctxAgentCert)
	e.lp(c.Person)
	e.lp(c.Device)
	e.lp(c.Agent)
	e.str(c.Name)
	e.u64(c.Issued)
	e.u64(c.Expires)
	e.count(len(c.Scopes))
	for _, s := range c.Scopes {
		e.str(s)
	}
	return e.b
}

func (c AgentCert) check() error {
	for _, k := range []struct {
		k    ed25519.PublicKey
		what string
	}{{c.Person, "person"}, {c.Device, "device"}, {c.Agent, "agent"}} {
		if err := checkKey(k.k, k.what); err != nil {
			return err
		}
	}
	if err := checkText(c.Name, "agent name", MaxNameLen); err != nil {
		return err
	}
	if c.Expires <= c.Issued {
		return invalid("agent certificate must expire after it is issued")
	}
	if c.Expires-c.Issued > uint64(MaxAgentTTL/time.Second) {
		return invalid("agent certificate lifetime exceeds %v", MaxAgentTTL)
	}
	if len(c.Scopes) == 0 || len(c.Scopes) > MaxScopes {
		return invalid("agent certificate needs 1..%d scopes", MaxScopes)
	}
	for _, s := range c.Scopes {
		if s == "" {
			return invalid("empty scope")
		}
		if err := checkText(s, "scope", maxValueLen); err != nil {
			return err
		}
	}
	return nil
}

// Sign fills Sig using d, which must be the key named in c.Device.
func (c *AgentCert) Sign(d ed25519.PrivateKey) error {
	if err := c.check(); err != nil {
		return err
	}
	sig, err := sign(d, c.Device, c.Body())
	c.Sig = sig
	return err
}

// Verify checks the certificate is well formed, within its lifetime limit,
// valid at now, and signed by c.Device. It does not check that c.Device
// belongs to c.Person; VerifyAgent does.
func (c AgentCert) Verify(now time.Time) error {
	if err := c.check(); err != nil {
		return err
	}
	if err := verifySig(c.Device, c.Body(), c.Sig); err != nil {
		return err
	}
	t := now.Unix()
	if t < 0 || uint64(t) >= c.Expires || c.Issued > uint64(now.Add(clockSkew).Unix()) {
		return ErrExpired
	}
	return nil
}

// Marshal returns the wire form: lp(body) | lp(sig).
func (c AgentCert) Marshal() []byte { return marshalSigned(c.Body(), c.Sig) }

// ParseAgentCert decodes a wire form. It does not verify the signature.
func ParseAgentCert(b []byte) (AgentCert, error) {
	body, sig, err := unmarshalSigned(b)
	if err != nil {
		return AgentCert{}, err
	}
	d := dec{b: body}
	d.expect(ctxAgentCert)
	c := AgentCert{Person: keyField(&d), Device: keyField(&d), Agent: keyField(&d), Name: d.str(),
		Issued: d.u64(), Expires: d.u64(), Sig: sig}
	n := d.count(MaxScopes)
	for i := 0; i < n && d.err == nil; i++ {
		c.Scopes = append(c.Scopes, d.str())
	}
	return c, d.done()
}

// Endpoint is one way to reach a device.
type Endpoint struct {
	Transport string // "tor", "wg", "direct", ...
	Address   string // onion address, host:port, ...
	Access    string // client-auth key, WireGuard public key, ...
}

// EndpointRecord says where a device can be reached. Signed by that device.
// The highest Seq wins.
type EndpointRecord struct {
	Device    ed25519.PublicKey // D_pub, the signer
	Seq       uint64
	Endpoints []Endpoint
	Sig       []byte
}

// Body returns the signed bytes.
func (r EndpointRecord) Body() []byte {
	var e enc
	e.str(ctxEndpoints)
	e.lp(r.Device)
	e.u64(r.Seq)
	e.count(len(r.Endpoints))
	for _, ep := range r.Endpoints {
		e.str(ep.Transport)
		e.str(ep.Address)
		e.str(ep.Access)
	}
	return e.b
}

func (r EndpointRecord) check() error {
	if err := checkKey(r.Device, "device"); err != nil {
		return err
	}
	if len(r.Endpoints) > MaxEndpoints {
		return invalid("more than %d endpoints", MaxEndpoints)
	}
	for _, ep := range r.Endpoints {
		for _, s := range []string{ep.Transport, ep.Address, ep.Access} {
			if err := checkText(s, "endpoint field", maxValueLen); err != nil {
				return err
			}
		}
	}
	return nil
}

// Sign fills Sig using d, which must be the key named in r.Device.
func (r *EndpointRecord) Sign(d ed25519.PrivateKey) error {
	if err := r.check(); err != nil {
		return err
	}
	sig, err := sign(d, r.Device, r.Body())
	r.Sig = sig
	return err
}

// Verify checks the record is well formed and signed by r.Device.
func (r EndpointRecord) Verify() error {
	if err := r.check(); err != nil {
		return err
	}
	return verifySig(r.Device, r.Body(), r.Sig)
}

// Marshal returns the wire form: lp(body) | lp(sig).
func (r EndpointRecord) Marshal() []byte { return marshalSigned(r.Body(), r.Sig) }

// ParseEndpointRecord decodes a wire form. It does not verify the signature.
func ParseEndpointRecord(b []byte) (EndpointRecord, error) {
	body, sig, err := unmarshalSigned(b)
	if err != nil {
		return EndpointRecord{}, err
	}
	d := dec{b: body}
	d.expect(ctxEndpoints)
	r := EndpointRecord{Device: keyField(&d), Seq: d.u64(), Sig: sig}
	n := d.count(MaxEndpoints)
	for i := 0; i < n && d.err == nil; i++ {
		r.Endpoints = append(r.Endpoints, Endpoint{Transport: d.str(), Address: d.str(), Access: d.str()})
	}
	return r, d.done()
}

// nextSeq picks the sequence number after prev: at least prev+1 and at least
// the current unix time. The time floor lets a device restored from the words,
// which knows no earlier seq, still issue one higher than any its contacts
// have cached (as long as its clock is not behind).
func nextSeq(prev uint64, now time.Time) uint64 {
	s := prev + 1
	if t := now.Unix(); t > 0 && uint64(t) > s {
		s = uint64(t)
	}
	return s
}

func unix(now time.Time) uint64 {
	if t := now.Unix(); t > 0 {
		return uint64(t)
	}
	return 0
}

// Allows reports whether the certificate's scopes permit running action (an "<app>.<verb>"
// name) on person's apps: AllowsAction, and the person limits. "person:<id>" scopes, when
// present, limit the people whose apps the agent may call (its own person included only if
// listed); without any, the agent may call any person its device can reach. Allows does not
// check validity or expiry.
func (c AgentCert) Allows(action, person string) bool {
	if !c.AllowsAction(action) {
		return false
	}
	limited := false
	for _, s := range c.Scopes {
		if p, ok := strings.CutPrefix(s, "person:"); ok {
			if p == person {
				return true
			}
			limited = true
		}
	}
	return !limited
}

// AllowsAction reports whether an action pattern among the scopes matches action: "*"
// (anything), "<app>.*" (every action of an app) or the exact action name.
func (c AgentCert) AllowsAction(action string) bool {
	app, _, _ := strings.Cut(action, ".")
	for _, s := range c.Scopes {
		if s == "*" || s == action || s == app+".*" {
			return true
		}
	}
	return false
}
