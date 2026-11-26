package daemon

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/session"
	"github.com/apv022/sol/sdk/go/sol"
)

// My profile (design/IDENTITY.md §Profile, ADMIN.md "Profile"): the name and picture my devices
// and contacts show for me, signed by P and synced by the session layer (session/profile.go).
// The daemon decodes no image: the Sol app crops and re-encodes a picture to a small square
// itself, and the daemon checks only its size and magic bytes.

// Where a shown name comes from.
const (
	NameFromPetname     = "petname"     // I renamed the contact myself
	NameFromProfile     = "profile"     // the name they chose (their profile)
	NameFromCertificate = "certificate" // the name in their person certificate (the last resort)
)

// ProfileTimeout bounds a profile change, which a device without P forwards to an admin device.
var ProfileTimeout = 30 * time.Second

// MeInfo is my profile as GET /admin/v1/me and the sol.profile.* actions show it.
type MeInfo struct {
	ID string `json:"id"`
	// Name is the name shown for me: my profile's, else my person certificate's.
	Name       string `json:"name"`
	NameSource string `json:"name_source"`
	// CertName is the name in my person certificate (signed by the recovery key).
	CertName string `json:"cert_name"`
	// Avatar is the URL of my picture on the UI root (the browser session is needed), and
	// AvatarHash its hash; both empty without a picture.
	Avatar     string `json:"avatar,omitempty"`
	AvatarHash string `json:"avatar_hash,omitempty"`
	// CanSign is true on an admin device (it holds P); any other device forwards a change to
	// an admin device of mine, which must be reachable.
	CanSign bool `json:"can_sign"`
}

// avatarURL is where the UI root serves a picture this device holds ("" if it holds none).
func (d *Daemon) avatarURL(hash string) string {
	if hash == "" || !d.Sessions.HasAvatar(hash) {
		return ""
	}
	return "/avatars/" + strings.TrimPrefix(hash, "sha256:")
}

// Me returns my profile.
func (d *Daemon) Me() MeInfo {
	mi := MeInfo{NameSource: NameFromCertificate, CanSign: d.Keyring.IsAdmin()}
	pk := d.Keyring.Person()
	if pk == nil {
		return mi
	}
	mi.ID = identity.ID(pk)
	if b, err := d.Keyring.Bundle(); err == nil {
		mi.CertName, mi.Name = b.Person.Name, b.Person.Name
	}
	if p, ok := d.Sessions.Profile(pk); ok {
		if p.Name != "" {
			mi.Name, mi.NameSource = p.Name, NameFromProfile
		}
		if p.Avatar != "" {
			mi.AvatarHash, mi.Avatar = p.Avatar, d.avatarURL(p.Avatar)
		}
	}
	return mi
}

// shownName returns the name to show for a contact and where it comes from: my petname for
// them if I set one, else their profile's name, else their certificate's.
func (d *Daemon) shownName(id, certName, petname string, renamed bool) (name, source, profileName, avatar string) {
	name, source = certName, NameFromCertificate
	if pub, err := identity.ParseID(id); err == nil {
		if p, ok := d.Sessions.Profile(pub); ok {
			profileName, avatar = p.Name, d.avatarURL(p.Avatar)
			if p.Name != "" {
				name, source = p.Name, NameFromProfile
			}
		}
	}
	if renamed && petname != "" {
		name, source = petname, NameFromPetname
	}
	if name == "" {
		name = petname
	}
	return name, source, profileName, avatar
}

// SetName changes my profile's name ("" shows my certificate's name again).
func (d *Daemon) SetName(ctx context.Context, name string) (MeInfo, error) {
	name = strings.TrimSpace(name)
	if len(name) > 64 {
		return MeInfo{}, sol.Errorf("bad_request", "a name is at most 64 characters")
	}
	return d.changeProfile(ctx, session.ProfileChange{Name: &name})
}

// SetAvatar makes b (JPEG, PNG or WebP, at most 64 KiB) my picture.
func (d *Daemon) SetAvatar(ctx context.Context, b []byte) (MeInfo, error) {
	if _, err := identity.AvatarType(b); err != nil {
		return MeInfo{}, sol.Errorf("bad_request", strings.TrimPrefix(err.Error(), "identity: invalid object: ")+
			" (the Sol app crops and shrinks a photo to a 256 px square for you)")
	}
	return d.changeProfile(ctx, session.ProfileChange{Avatar: b})
}

// ClearAvatar removes my picture (the letter avatar shows again).
func (d *Daemon) ClearAvatar(ctx context.Context) (MeInfo, error) {
	return d.changeProfile(ctx, session.ProfileChange{ClearAvatar: true})
}

func (d *Daemon) changeProfile(ctx context.Context, c session.ProfileChange) (MeInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, ProfileTimeout)
	defer cancel()
	if _, err := d.Sessions.SetProfile(ctx, c); err != nil {
		switch {
		case errors.Is(err, session.ErrNoAdmin):
			return MeInfo{}, sol.Errorf("unavailable", strings.TrimPrefix(err.Error(), "session: "))
		case errors.Is(err, identity.ErrInvalid):
			return MeInfo{}, sol.Errorf("bad_request", strings.TrimPrefix(err.Error(), "identity: invalid object: "))
		}
		return MeInfo{}, err
	}
	return d.Me(), nil
}

// profileChanged is session.Config.OnProfile: the shown name or picture of me or a contact
// changed, so the Sol app (and apps that follow people) hear of it.
func (d *Daemon) profileChanged(person ed25519.PublicKey) {
	if pk := d.Keyring.Person(); pk != nil && string(pk) == string(person) {
		go d.emitStatus()
		return
	}
	if d.API != nil {
		d.API.PersonChanged(identity.ID(person))
	}
}

// serveAvatar serves GET /avatars/{hash} on the UI root: a picture this device holds (mine or a
// contact's), to a signed-in browser only, with its real type and nosniff.
func (d *Daemon) serveAvatar(w http.ResponseWriter, r *http.Request) {
	if !d.hasSession(r) {
		sol.WriteError(w, sol.Errorf("unauthorized", "This browser isn't signed in to Sol."))
		return
	}
	b, t, ok := d.Sessions.Avatar("sha256:" + r.PathValue("hash"))
	if !ok {
		sol.WriteError(w, sol.Errorf("not_found", "no such picture"))
		return
	}
	w.Header().Set("Content-Type", t)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// A hash names immutable bytes.
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Write(b)
}

// readAvatarBody reads a raw picture upload (PUT /admin/v1/me/avatar).
func readAvatarBody(r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, identity.MaxAvatarSize+1))
	if err != nil {
		return nil, sol.Errorf("bad_request", "reading the picture failed")
	}
	if len(b) > identity.MaxAvatarSize {
		return nil, sol.Errorf("bad_request", "the picture is larger than 64 KiB")
	}
	return b, nil
}

// avatarArg is the picture sol.profile.avatar was given: base64 `data`, or the absolute `path`
// of an image file on this computer.
func avatarArg(args map[string]string) ([]byte, error) {
	data, path := strings.TrimSpace(args["data"]), strings.TrimSpace(args["path"])
	switch {
	case data != "" && path != "":
		return nil, sol.Errorf("bad_request", "give data or path, not both")
	case data != "":
		data = data[strings.Index(data, ",")+1:] // a data: URL works too
		b, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			if b, err = base64.RawStdEncoding.DecodeString(data); err != nil {
				return nil, sol.Errorf("bad_request", "data is not base64")
			}
		}
		return b, nil
	case path != "":
		if !filepath.IsAbs(path) {
			return nil, sol.Errorf("bad_request", "path must be absolute")
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, sol.Errorf("not_found", "cannot open "+path)
		}
		defer f.Close()
		if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
			return nil, sol.Errorf("bad_request", path+" is not a file")
		}
		b, err := io.ReadAll(io.LimitReader(f, identity.MaxAvatarSize+1))
		if err != nil {
			return nil, sol.Errorf("bad_request", "reading "+path+" failed")
		}
		return b, nil
	}
	return nil, sol.Errorf("bad_request", "give data (base64) or path (an image file)")
}
