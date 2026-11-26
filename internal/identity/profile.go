package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Profiles (IDENTITY.md §Profile): how a person wants to be shown, a display name and a small
// picture, signed by P. The person certificate's name is signed by R, which no device keeps once
// the backup is confirmed, so a rename must not need it; P is on every admin device. A profile is
// never part of the bundle (older devices parse a bundle as exactly three parts), so it travels
// on its own session endpoints (session/profile.go).

const ctxProfile = "sol profile v1"

// MaxAvatarSize bounds a profile picture: the client re-encodes it to a 256 px square, which is
// far smaller, and the daemon never decodes it (it only checks the magic bytes and the size).
const MaxAvatarSize = 64 << 10

// Profile is a person's signed display name and picture. The highest Seq wins. Avatar is
// AvatarHash of the picture, or "" for none. A profile is valid only under the P of the newest
// person certificate: one signed by an older P stops counting once a newer certificate is seen.
type Profile struct {
	Person ed25519.PublicKey // R_pub: whose profile
	Seq    uint64
	Name   string // "" means "use the certificate's name"
	Avatar string // "sha256:<hex>" or ""
	Sig    []byte // by P
}

// Body returns the signed bytes:
// lp("sol profile v1") | lp(R_pub) | u64(seq) | lp(name) | lp(avatar).
func (p Profile) Body() []byte {
	var e enc
	e.str(ctxProfile)
	e.lp(p.Person)
	e.u64(p.Seq)
	e.str(p.Name)
	e.str(p.Avatar)
	return e.b
}

func (p Profile) check() error {
	if err := checkKey(p.Person, "person"); err != nil {
		return err
	}
	if err := checkText(p.Name, "profile name", MaxNameLen); err != nil {
		return err
	}
	if p.Avatar != "" && !validAvatarHash(p.Avatar) {
		return invalid("avatar must be sha256:<64 hex digits> or empty")
	}
	return nil
}

func validAvatarHash(h string) bool {
	x, ok := strings.CutPrefix(h, "sha256:")
	if !ok || len(x) != 64 || strings.ToLower(x) != x {
		return false
	}
	_, err := hex.DecodeString(x)
	return err == nil
}

// ValidAvatarHash reports whether h is a well-formed avatar hash ("sha256:" and 64 lowercase
// hex digits), so a hash taken from a URL can be used as a file name.
func ValidAvatarHash(h string) bool { return validAvatarHash(h) }

// Sign fills Sig with the person key p.
func (p *Profile) Sign(key ed25519.PrivateKey) error {
	if err := p.check(); err != nil {
		return err
	}
	if len(key) != ed25519.PrivateKeySize {
		return invalid("private key must be %d bytes", ed25519.PrivateKeySize)
	}
	p.Sig = ed25519.Sign(key, p.Body())
	return nil
}

// Verify checks the profile is well formed and signed by the P that pc (the newest verified
// person certificate known for that person) names.
func (p Profile) Verify(pc PersonCert) error {
	if err := p.check(); err != nil {
		return err
	}
	if !bytes.Equal(p.Person, pc.Person) {
		return invalid("profile is for another person")
	}
	return verifySig(pc.Key, p.Body(), p.Sig)
}

// Marshal returns the wire form: lp(body) | lp(sig).
func (p Profile) Marshal() []byte { return marshalSigned(p.Body(), p.Sig) }

// ParseProfile decodes a wire form. It does not verify the signature.
func ParseProfile(b []byte) (Profile, error) {
	body, sig, err := unmarshalSigned(b)
	if err != nil {
		return Profile{}, err
	}
	d := dec{b: body}
	d.expect(ctxProfile)
	p := Profile{Person: keyField(&d), Seq: d.u64(), Name: d.str(), Avatar: d.str(), Sig: sig}
	if err := d.done(); err != nil {
		return Profile{}, err
	}
	return p, p.check()
}

// Anonymous reports whether p is the form an anonymous contact may receive (no picture).
func (p Profile) Anonymous() bool { return p.Avatar == "" }

// CompareProfile orders a received profile against the one held: 1 if next replaces cur, 0 if it
// is the same (or adds nothing), and ErrRollback or ErrConflict otherwise. At an equal seq the
// form with a picture replaces the form without one when the names agree: both forms of one seq
// are signed by P, and a contact switched from anonymous to buddy gets the full form at the seq
// it already holds (IDENTITY.md §Profile).
func CompareProfile(cur, next Profile) (int, error) {
	switch {
	case next.Seq > cur.Seq:
		return 1, nil
	case next.Seq < cur.Seq:
		return 0, fmt.Errorf("profile: %w", ErrRollback)
	case bytes.Equal(next.Marshal(), cur.Marshal()):
		return 0, nil
	case next.Name == cur.Name && cur.Avatar == "" && next.Avatar != "":
		return 1, nil
	case next.Name == cur.Name && next.Avatar == "" && cur.Avatar != "":
		return 0, nil // the anonymous form of what is held
	}
	return 0, fmt.Errorf("profile: %w", ErrConflict)
}

// AvatarHash returns the hash a profile names a picture by: "sha256:<hex>".
func AvatarHash(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// AvatarType checks a picture's size (at most MaxAvatarSize, not empty) and its magic bytes,
// and returns its media type: image/jpeg, image/png or image/webp. Nothing is decoded.
func AvatarType(b []byte) (string, error) {
	switch {
	case len(b) == 0:
		return "", invalid("the picture is empty")
	case len(b) > MaxAvatarSize:
		return "", invalid("the picture is larger than %d KiB", MaxAvatarSize>>10)
	case bytes.HasPrefix(b, []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg", nil
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png", nil
	case len(b) > 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp", nil
	}
	return "", invalid("the picture must be a JPEG, PNG or WebP image")
}

// SignProfile signs this person's profile with P at the seq after prev: the full form (name and
// avatar) and the form for anonymous contacts (the same seq, avatar ""), which is byte-identical
// when there is no picture. Admin devices only (ErrNotAdmin): this is how a rename avoids the
// words.
func (k *Keyring) SignProfile(prev uint64, name, avatar string, now time.Time) (full, anon Profile, err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.st.pc == nil {
		return Profile{}, Profile{}, ErrNoPerson
	}
	if k.st.person == nil {
		return Profile{}, Profile{}, ErrNotAdmin
	}
	seq := nextSeq(prev, now)
	full = Profile{Person: k.st.pc.Person, Seq: seq, Name: name, Avatar: avatar}
	if err := full.Sign(k.st.person); err != nil {
		return Profile{}, Profile{}, err
	}
	anon = Profile{Person: k.st.pc.Person, Seq: seq, Name: name}
	if err := anon.Sign(k.st.person); err != nil {
		return Profile{}, Profile{}, err
	}
	return full, anon, nil
}
