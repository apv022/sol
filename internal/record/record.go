// Package record defines the signed service record: a public statement that
// one key publishes one service at a set of addresses.
//
// A public service has no Sol handshake (visitors use Tor Browser), so until
// now nothing tied its addresses together or to a lasting identity. The
// record does. It is what a directory lists, what a discovery channel
// (Nostr, later) carries, and what lets a visitor check that an onion and
// an I2P address are the same site.
//
// Wire format, served at Path on every address of the service:
//
//	{"record":{"v":1,"name":"blog","pubkey":"<base64 ed25519>","addrs":{"tor":"x.onion"}},
//	 "sig":"<base64 ed25519 signature>"}
//
// The signature covers sigContext followed by the "record" value with
// insignificant whitespace removed (json.Compact), so a verifier checks the
// bytes it received and needs no canonical JSON rules.
//
// Proof of control runs both ways: the key holder names the addresses, and
// an address proves it agrees by serving the record. A verifier must fetch
// the record from an address and check Record.Reaches for that address.
//
// Invariants:
//   - The signing key belongs to the service alone. It is never the node
//     key: a public service must not be linkable to the node (docs/ARCHITECTURE.md).
//   - Records are opt-in per service. Serving one tells visitors the site
//     runs Sol.
package record

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/apv022/sol/internal/identity"
)

const (
	// Path is where a service serves its record.
	Path = "/.well-known/sol.json"
	// Version is the record format this build writes and accepts.
	Version = 1
	// MaxSize bounds a record a verifier will parse.
	MaxSize = 16 << 10

	// sigContext keeps a record signature from being valid as anything else
	// signed with the same key.
	sigContext = "sol service record v1\n"
)

// Record is the signed statement.
type Record struct {
	V      int               `json:"v"`
	Name   string            `json:"name"`   // the service's name, chosen by its owner
	PubKey string            `json:"pubkey"` // the service key (base64 ed25519)
	Addrs  map[string]string `json:"addrs"`  // transport name -> public address
}

type envelope struct {
	Record json.RawMessage `json:"record"`
	Sig    string          `json:"sig"`
}

// Reaches reports whether addr is one of the record's addresses.
func (r Record) Reaches(addr string) bool {
	for _, a := range r.Addrs {
		if a == addr {
			return true
		}
	}
	return false
}

// Sign builds the record for a service and signs it with key.
func Sign(key *identity.Identity, name string, addrs map[string]string) ([]byte, error) {
	if len(addrs) == 0 {
		return nil, errors.New("record: a service with no address has nothing to announce")
	}
	body, err := json.Marshal(Record{V: Version, Name: name, PubKey: identity.EncodePublicKey(key.Public()), Addrs: addrs})
	if err != nil {
		return nil, err
	}
	sig := key.Sign(append([]byte(sigContext), body...))
	return json.Marshal(envelope{Record: body, Sig: base64.StdEncoding.EncodeToString(sig)})
}

// Verify parses a signed record and checks its signature against the key
// inside it. It does not prove the key controls the addresses: fetch the
// record from an address and check Reaches (see the package doc).
func Verify(data []byte) (Record, error) {
	if len(data) > MaxSize {
		return Record{}, fmt.Errorf("record: larger than %d bytes", MaxSize)
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return Record{}, fmt.Errorf("record: %w", err)
	}
	var rec Record
	if err := json.Unmarshal(env.Record, &rec); err != nil {
		return Record{}, fmt.Errorf("record: %w", err)
	}
	if rec.V != Version {
		return Record{}, fmt.Errorf("record: unsupported version %d", rec.V)
	}
	pub, err := identity.DecodePublicKey(rec.PubKey)
	if err != nil {
		return Record{}, fmt.Errorf("record: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(env.Sig)
	if err != nil {
		return Record{}, errors.New("record: signature is not base64")
	}
	var body bytes.Buffer
	body.WriteString(sigContext)
	if err := json.Compact(&body, env.Record); err != nil {
		return Record{}, fmt.Errorf("record: %w", err)
	}
	if !identity.Verify(pub, body.Bytes(), sig) {
		return Record{}, errors.New("record: bad signature")
	}
	return rec, nil
}

// Handler serves the record at Path and passes every other request to next.
// current returns the signed record, or nil while there is none (the path is
// then "not found", and never reaches the app behind next).
func Handler(next http.Handler, current func() []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != Path {
			next.ServeHTTP(w, r)
			return
		}
		rec := current()
		if rec == nil || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(rec)
	})
}
