package apphost

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/apv022/sol/internal/fsutil"
	"github.com/apv022/sol/sdk/go/sol"
)

// External apps (design/APPS_EVERYWHERE.md M5) are programs Sol does not run: a separate
// Android APK, say, that asked to be a Sol app and was approved. Each is an app directory
// <apps>/<id>/ like any other, holding its app.json (exec "external", or
// "external:android:<package>") and external.json (who it is bound to and the SHA-256 of its
// one long-lived token). The token itself is shown once, to the registering caller, and never
// stored: at every start the host grants its hash again (Tokens.Grant). An external app has no
// UI and no folders (it cannot reach the synced folder's path) and is never started or
// supervised; removing it revokes the token.
//
// An external app may also have icon.png beside its binding: its launcher icon as the Android
// shell read it from PackageManager (never from what the app claims), validated here and
// replaced, or removed, at every registration, so it is always the icon of the last connect.

// ExternalFile is the binding file of an external app, inside its app directory.
const ExternalFile = "external.json"

// ExternalIconFile is an external app's icon, inside its app directory.
const ExternalIconFile = "icon.png"

// Limits of an external app's icon (the Android shell sends 192 px).
const (
	MaxExternalIconBytes = 256 << 10
	MaxExternalIconSide  = 512
)

// pngMagic starts every PNG file.
var pngMagic = []byte("\x89PNG\r\n\x1a\n")

// ValidateIconPNG checks an external app's icon: a PNG of at most MaxExternalIconBytes whose
// header decodes to a size within MaxExternalIconSide on each side. Only the header is decoded:
// the bytes are served as they are, with the PNG type and nosniff.
func ValidateIconPNG(b []byte) error {
	if len(b) > MaxExternalIconBytes {
		return sol.Errorf("bad_request", "the icon is larger than 256 KB")
	}
	if !bytes.HasPrefix(b, pngMagic) {
		return sol.Errorf("bad_request", "the icon is not a PNG")
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return sol.Errorf("bad_request", "the icon is not a valid PNG")
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > MaxExternalIconSide || cfg.Height > MaxExternalIconSide {
		return sol.Errorf("bad_request", "the icon must be at most 512 by 512 pixels")
	}
	return nil
}

// ExternalBinding is what an external app is bound to (external.json).
type ExternalBinding struct {
	// Package is the Android package name ("" for an external app that is not an APK).
	Package string `json:"package,omitempty"`
	// Cert is the SHA-256 of the package's signing certificate, lowercase hex.
	Cert string `json:"cert,omitempty"`
	// TokenSHA256 is the SHA-256 of the app's token, lowercase hex.
	TokenSHA256 string `json:"token_sha256"`
}

// androidPackage is an Android package name: dot-separated Java identifiers, at least two.
var androidPackage = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*)+$`)

// certHex is a SHA-256 in lowercase hex.
var certHex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// isExternal reports whether exec names an external app.
func isExternal(exec string) bool { return exec == "external" || strings.HasPrefix(exec, "external:") }

// RegisterExternal registers (or, for the same binding, re-registers) an external app from its
// manifest and returns the manifest as stored and a new token, the only time the token exists
// outside the caller. pkg and cert bind it to an Android package and signing certificate
// (both, or neither for a non-Android program). Exec, UI and folders of m are ignored or
// refused: exec is set to "external[:android:<pkg>]", a UI or folders are an error. icon is
// the app's launcher icon as a PNG (ValidateIconPNG) or nil; it replaces the stored one, and
// nil removes it.
//
// It refuses the id of an installed app that is not external, the id of an external app
// bound to another package or certificate, and a package already bound under another
// certificate (a different signer is a different app). Re-registering revokes every earlier
// token of the app.
func (h *Host) RegisterExternal(m *sol.Manifest, pkg, cert string, icon []byte) (*sol.Manifest, string, error) {
	if len(icon) > 0 {
		if err := ValidateIconPNG(icon); err != nil {
			return nil, "", err
		}
	}
	if (pkg == "") != (cert == "") {
		return nil, "", sol.Errorf("bad_request", "package and cert go together")
	}
	if pkg != "" && !androidPackage.MatchString(pkg) {
		return nil, "", sol.Errorf("bad_request", "bad package name")
	}
	cert = strings.ToLower(strings.ReplaceAll(cert, ":", ""))
	if pkg != "" && !certHex.MatchString(cert) {
		return nil, "", sol.Errorf("bad_request", "cert must be the SHA-256 of the signing certificate, in hex")
	}
	m.Exec = "external"
	if pkg != "" {
		m.Exec = "external:android:" + pkg
	}
	if m.Version == "" {
		m.Version = "1"
	}
	if m.UI != "" {
		return nil, "", sol.Errorf("bad_request", "an external app has no UI served by Sol")
	}
	if len(m.Folders) > 0 {
		return nil, "", sol.Errorf("bad_request", "folders are not available to external apps (use blobs)")
	}
	if err := m.Validate(); err != nil {
		return nil, "", sol.Errorf("bad_request", err.Error())
	}
	if err := h.Validate(m, ""); err != nil {
		return nil, "", sol.Errorf("bad_request", err.Error())
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, "", err
	}
	token, sum := newExternalToken()
	bind, _ := json.MarshalIndent(ExternalBinding{Package: pkg, Cert: cert, TokenSHA256: hex.EncodeToString(sum[:])}, "", "  ")

	for _, b := range h.cfg.Builtins {
		// A first-party app's id stays its own even while it is removed.
		if bm, err := sol.ParseManifest(b.Manifest); err == nil && bm.ID == m.ID {
			return nil, "", sol.Errorf("conflict", m.ID+" is the id of a Sol app")
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if old := h.apps[m.ID]; old != nil {
		if !isExternal(old.m.Exec) {
			return nil, "", sol.Errorf("conflict", "an installed app already has the id "+m.ID)
		}
		if b, _ := readBinding(old.dir); b.Package != pkg || b.Cert != cert {
			return nil, "", sol.Errorf("conflict", m.ID+" is registered by another app")
		}
	}
	if pkg != "" {
		for id, a := range h.apps {
			if !isExternal(a.m.Exec) || id == m.ID {
				continue
			}
			if b, _ := readBinding(a.dir); b.Package == pkg && b.Cert != cert {
				return nil, "", sol.Errorf("conflict", "package "+pkg+" is registered with another signing certificate")
			}
		}
	}
	dir := filepath.Join(h.cfg.AppsDir, m.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	// The binding (with the new hash) first: a crash between the two writes leaves the old
	// manifest with the new token, never a manifest without a binding.
	if err := fsutil.WriteFileAtomic(filepath.Join(dir, ExternalFile), bind, 0o600); err != nil {
		return nil, "", err
	}
	if err := fsutil.WriteFileAtomic(filepath.Join(dir, "app.json"), raw, 0o600); err != nil {
		return nil, "", err
	}
	if len(icon) > 0 {
		if err := fsutil.WriteFileAtomic(filepath.Join(dir, ExternalIconFile), icon, 0o600); err != nil {
			return nil, "", err
		}
	} else if err := os.Remove(filepath.Join(dir, ExternalIconFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, "", err
	}
	_ = os.Remove(RemovedMarker(h.cfg.AppsDir, m.ID))
	if h.apps[m.ID] != nil {
		h.cfg.Tokens.Revoke(m.ID) // the earlier token (and bridge token) go
	}
	h.add(m, dir)
	h.cfg.Tokens.Grant(m.ID, sum)
	if h.cfg.OnChange != nil {
		go h.cfg.OnChange(m.ID) // h.mu is held here
	}
	return m, token, nil
}

// External returns the binding of external app id.
func (h *Host) External(id string) (ExternalBinding, bool) {
	h.mu.Lock()
	a := h.apps[id]
	h.mu.Unlock()
	if a == nil || !isExternal(a.m.Exec) {
		return ExternalBinding{}, false
	}
	b, err := readBinding(a.dir)
	return b, err == nil
}

// ExternalIcon returns the path and modification time of external app id's icon.png; ok is
// false when the app is not external or has no icon.
func (h *Host) ExternalIcon(id string) (path string, mod time.Time, ok bool) {
	h.mu.Lock()
	a := h.apps[id]
	h.mu.Unlock()
	if a == nil || !isExternal(a.m.Exec) {
		return "", time.Time{}, false
	}
	p := filepath.Join(a.dir, ExternalIconFile)
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return "", time.Time{}, false
	}
	return p, fi.ModTime(), true
}

// HasExternal reports whether any external app is registered.
func (h *Host) HasExternal() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, a := range h.apps {
		if isExternal(a.m.Exec) {
			return true
		}
	}
	return false
}

// grantExternal grants an external app's stored token hash again (at load). An app without a
// readable binding keeps no token: it shows as installed and can be removed or registered again.
func (h *Host) grantExternal(id, dir string) {
	b, err := readBinding(dir)
	if err != nil {
		h.log.Warn("external app without a binding; it has no token", "app", id, "err", err)
		return
	}
	raw, err := hex.DecodeString(b.TokenSHA256)
	if err != nil || len(raw) != sha256.Size {
		h.log.Warn("external app with a bad token hash; it has no token", "app", id)
		return
	}
	h.cfg.Tokens.Grant(id, [sha256.Size]byte(raw))
}

func readBinding(dir string) (ExternalBinding, error) {
	var b ExternalBinding
	raw, err := os.ReadFile(filepath.Join(dir, ExternalFile))
	if err != nil {
		return b, err
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return b, errors.New("apphost: bad " + ExternalFile)
	}
	return b, nil
}

// newExternalToken returns a new random token and its SHA-256.
func newExternalToken() (string, [32]byte) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	t := hex.EncodeToString(b)
	return t, sha256.Sum256([]byte(t))
}
