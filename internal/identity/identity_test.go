package identity

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadOrCreatePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.key")
	a, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Public(), b.Public()) {
		t.Fatal("reloaded key differs")
	}
	if runtime.GOOS == "windows" {
		return // no Unix permission bits
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode = %v, want 0600", st.Mode().Perm())
	}
}

func TestSignVerify(t *testing.T) {
	id, _ := Generate()
	msg := []byte("hello")
	sig := id.Sign(msg)
	if !Verify(id.Public(), msg, sig) {
		t.Fatal("valid signature rejected")
	}
	if Verify(id.Public(), []byte("hellO"), sig) {
		t.Fatal("tampered message accepted")
	}
	if Verify(nil, msg, sig) || Verify(id.Public(), msg, sig[:10]) {
		t.Fatal("malformed input accepted")
	}
}

func TestNodeIDRoundTrip(t *testing.T) {
	id, _ := Generate()
	pub, err := ParseNodeID(id.NodeID())
	if err != nil || !bytes.Equal(pub, id.Public()) {
		t.Fatalf("round trip failed: %v", err)
	}
	if len(id.DeviceID()) != 16 {
		t.Fatalf("device id length %d", len(id.DeviceID()))
	}
}

func TestPublicKeyEncoding(t *testing.T) {
	id, _ := Generate()
	pub, err := DecodePublicKey(EncodePublicKey(id.Public()))
	if err != nil || !bytes.Equal(pub, id.Public()) {
		t.Fatal("round trip failed")
	}
	if _, err := DecodePublicKey("bm9wZQ=="); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestX25519Formats(t *testing.T) {
	k, err := NewX25519()
	if err != nil {
		t.Fatal(err)
	}
	if len(k.TorPublic()) != 52 || len(k.TorPrivate()) != 44 {
		t.Fatalf("unexpected tor key lengths: %q %q", k.TorPublic(), k.TorPrivate())
	}
}
