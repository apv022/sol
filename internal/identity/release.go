package identity

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// App release records (design/CHANNELS.md): a channel is an ed25519 key C that a developer makes
// once, and a release record is one version of one app signed by C. The record names each
// artifact by its SHA-256, so the bytes may come from anywhere (the channel's onion, a mirror):
// the signature, not the host, says what a release is. A channel key is not a person or device
// key; nothing here ties it to an identity.

const ctxRelease = "sol app-release v1"

// Artifact kinds.
const (
	// KindSolApp is a tar of a Sol app directory (app.json, the executable, the UI).
	KindSolApp = "sol-app"
	// KindAPK is an Android package, installed by the Android shell.
	KindAPK = "apk"
)

// Bounds of a release record. They keep a record well under a feed entry's body (it travels
// base64 inside one, feeds.MaxBodySize is 64 KiB) and under maxField.
const (
	MaxReleaseNotes   = 8 << 10
	MaxArtifacts      = 8
	MaxMirrors        = 4
	MaxMirrorURL      = 512
	MaxReleaseVersion = 64
	// MaxReleaseSize bounds a record's wire form.
	MaxReleaseSize = 40 << 10
)

var (
	releaseAppRe      = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`) // sol.ValidAppID
	releasePlatformRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	releaseVersionRe  = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]*$`)
)

// Artifact is one file of a release: a build for one platform.
type Artifact struct {
	// Platform is GOOS-GOARCH ("linux-amd64", "darwin-arm64", "windows-amd64") or "android".
	Platform string
	// Kind is KindSolApp or KindAPK.
	Kind string
	// SHA256 is the artifact's SHA-256 (32 bytes).
	SHA256 []byte
	// Size is its length in bytes.
	Size uint64
	// Mirrors are optional https:// URLs serving the same bytes.
	Mirrors []string
}

// Hash returns the artifact's SHA-256 as lowercase hex.
func (a Artifact) Hash() string { return hex.EncodeToString(a.SHA256) }

// Release is a release record. For one (channel, app) the highest Seq wins; a record with a
// lower or equal Seq is never taken over the one held (CompareRelease).
type Release struct {
	Channel   ed25519.PublicKey // C_pub
	Seq       uint64
	App       string // the app's manifest id
	Version   string // the manifest version it carries
	Time      uint64 // unix seconds, when it was signed
	Notes     string
	Artifacts []Artifact
	Sig       []byte // by C
}

// Body returns the signed bytes:
//
//	lp("sol app-release v1") | lp(C_pub) | u64(seq) | lp(app_id) | lp(version) | u64(time)
//	  | lp(notes) | list of artifact: lp(platform) | lp(kind) | lp(sha256) | u64(size)
//	                                  | list of lp(mirror_url)
//
// sha256 is the 32 raw bytes of the digest.
func (r Release) Body() []byte {
	var e enc
	e.str(ctxRelease)
	e.lp(r.Channel)
	e.u64(r.Seq)
	e.str(r.App)
	e.str(r.Version)
	e.u64(r.Time)
	e.str(r.Notes)
	e.count(len(r.Artifacts))
	for _, a := range r.Artifacts {
		e.str(a.Platform)
		e.str(a.Kind)
		e.lp(a.SHA256)
		e.u64(a.Size)
		e.count(len(a.Mirrors))
		for _, m := range a.Mirrors {
			e.str(m)
		}
	}
	return e.b
}

func (r Release) check() error {
	if err := checkKey(r.Channel, "channel"); err != nil {
		return err
	}
	switch {
	case r.Seq == 0:
		return invalid("release seq must be at least 1")
	case !releaseAppRe.MatchString(r.App):
		return invalid("release app id %q is not a valid app id", r.App)
	case len(r.Version) > MaxReleaseVersion || !releaseVersionRe.MatchString(r.Version):
		return invalid("release version must be 1-%d letters, digits and . + _ -", MaxReleaseVersion)
	case len(r.Artifacts) == 0 || len(r.Artifacts) > MaxArtifacts:
		return invalid("a release has 1-%d artifacts", MaxArtifacts)
	}
	if err := checkText(r.Notes, "release notes", MaxReleaseNotes); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, a := range r.Artifacts {
		if !releasePlatformRe.MatchString(a.Platform) {
			return invalid("artifact platform %q", a.Platform)
		}
		if a.Kind != KindSolApp && a.Kind != KindAPK {
			return invalid("artifact kind must be %s or %s", KindSolApp, KindAPK)
		}
		if seen[a.Platform+"/"+a.Kind] {
			return invalid("two %s artifacts for %s", a.Kind, a.Platform)
		}
		seen[a.Platform+"/"+a.Kind] = true
		if len(a.SHA256) != 32 {
			return invalid("artifact sha256 must be 32 bytes")
		}
		if a.Size == 0 {
			return invalid("artifact size must be at least 1")
		}
		if len(a.Mirrors) > MaxMirrors {
			return invalid("an artifact has at most %d mirrors", MaxMirrors)
		}
		for _, m := range a.Mirrors {
			if !ValidMirrorURL(m) {
				return invalid("mirror %q must be an https:// URL of at most %d bytes", m, MaxMirrorURL)
			}
		}
	}
	if len(r.Marshal()) > MaxReleaseSize {
		return invalid("release record larger than %d KiB", MaxReleaseSize>>10)
	}
	return nil
}

// ValidMirrorURL reports whether m is an acceptable mirror: an absolute https:// URL with a
// host, no user info, at most MaxMirrorURL bytes.
func ValidMirrorURL(m string) bool {
	if len(m) > MaxMirrorURL || strings.ContainsAny(m, " \t\r\n") {
		return false
	}
	u, err := url.Parse(m)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil
}

// Sign fills Channel and Sig with the channel key c, after checking the record is well formed.
func (r *Release) Sign(c ed25519.PrivateKey) error {
	if len(c) != ed25519.PrivateKeySize {
		return invalid("private key must be %d bytes", ed25519.PrivateKeySize)
	}
	r.Channel = c.Public().(ed25519.PublicKey)
	if err := r.check(); err != nil {
		return err
	}
	r.Sig = ed25519.Sign(c, r.Body())
	return nil
}

// Verify checks the record is well formed and signed by the channel key it names. Whether that
// key is one the caller follows (or pinned) is the caller's question: VerifyFrom.
func (r Release) Verify() error {
	if err := r.check(); err != nil {
		return err
	}
	return verifySig(r.Channel, r.Body(), r.Sig)
}

// VerifyFrom is Verify plus: the record is of channel c.
func (r Release) VerifyFrom(c ed25519.PublicKey) error {
	if !bytes.Equal(r.Channel, c) {
		return invalid("release is of another channel")
	}
	return r.Verify()
}

// Marshal returns the wire form: lp(body) | lp(sig).
func (r Release) Marshal() []byte { return marshalSigned(r.Body(), r.Sig) }

// ParseRelease decodes a wire form. It does not verify the signature.
func ParseRelease(b []byte) (Release, error) {
	if len(b) > MaxReleaseSize {
		return Release{}, invalid("release record larger than %d KiB", MaxReleaseSize>>10)
	}
	body, sig, err := unmarshalSigned(b)
	if err != nil {
		return Release{}, err
	}
	d := dec{b: body}
	d.expect(ctxRelease)
	r := Release{Channel: keyField(&d), Seq: d.u64(), App: d.str(), Version: d.str(), Time: d.u64(), Notes: d.str(), Sig: sig}
	n := d.count(MaxArtifacts)
	for range n {
		a := Artifact{Platform: d.str(), Kind: d.str(), SHA256: d.lp(), Size: d.u64()}
		m := d.count(MaxMirrors)
		for range m {
			a.Mirrors = append(a.Mirrors, d.str())
		}
		r.Artifacts = append(r.Artifacts, a)
	}
	if err := d.done(); err != nil {
		return Release{}, err
	}
	return r, r.check()
}

// CompareRelease orders a received record against the one held for the same channel and app:
// 1 if next replaces cur, 0 if it is the same record, ErrRollback for a lower seq and
// ErrConflict for a different record at the same seq (a channel key signing two releases with
// one seq is misbehaving; the first one seen stays).
func CompareRelease(cur, next Release) (int, error) {
	switch {
	case !bytes.Equal(cur.Channel, next.Channel) || cur.App != next.App:
		return 0, invalid("releases of different channels or apps do not compare")
	case next.Seq > cur.Seq:
		return 1, nil
	case next.Seq < cur.Seq:
		return 0, ErrRollback
	case bytes.Equal(cur.Marshal(), next.Marshal()):
		return 0, nil
	}
	return 0, ErrConflict
}

// NextReleaseSeq is the seq a new release after prev gets: prev+1, or the current unix time
// when that is larger, so a channel key used from CI (which may not know prev) still moves
// forward.
func NextReleaseSeq(prev uint64, now time.Time) uint64 { return nextSeq(prev, now) }
