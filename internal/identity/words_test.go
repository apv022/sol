package identity

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// Vectors from the BIP-39 reference test set (trezor/python-mnemonic
// vectors.json), 256-bit entries.
var bip39Vectors = []struct{ entropy, words string }{
	{"0000000000000000000000000000000000000000000000000000000000000000",
		strings.Repeat("abandon ", 23) + "art"},
	{"7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f",
		"legal winner thank year wave sausage worth useful legal winner thank year wave sausage worth useful legal winner thank year wave sausage worth title"},
	{"8080808080808080808080808080808080808080808080808080808080808080",
		"letter advice cage absurd amount doctor acoustic avoid letter advice cage absurd amount doctor acoustic avoid letter advice cage absurd amount doctor acoustic bless"},
	{"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		strings.Repeat("zoo ", 23) + "vote"},
	{"68a79eaca2324873eacc50cb9c6eca8cc68ea5d936f98787c60c7ebc74e6ce7c",
		"hamster diagram private dutch cause delay private meat slide toddler razor book happy fancy gospel tennis maple dilemma loan word shrug inflict delay length"},
}

func TestBIP39Vectors(t *testing.T) {
	for _, v := range bip39Vectors {
		ent, _ := hex.DecodeString(v.entropy)
		w, err := EntropyToWords(ent)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(w, " "); got != v.words {
			t.Errorf("%s:\ngot  %s\nwant %s", v.entropy, got, v.words)
		}
		back, err := ParseWords("  " + strings.ToUpper(v.words) + "\n")
		if err != nil || !bytes.Equal(back, ent) {
			t.Errorf("%s: decode: %v", v.entropy, err)
		}
	}
}

func TestWordsRejects(t *testing.T) {
	good := strings.Fields(bip39Vectors[1].words)
	swapped := append([]string{}, good...)
	swapped[0], swapped[1] = swapped[1], swapped[0] // checksum breaks
	unknown := append([]string{}, good...)
	unknown[3] = "solx"
	for name, w := range map[string][]string{
		"short": good[:23], "long": append(append([]string{}, good...), "zoo"),
		"checksum": swapped, "unknown": unknown,
	} {
		if _, err := WordsToEntropy(w); !errors.Is(err, ErrBadWords) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	if _, err := EntropyToWords(make([]byte, 16)); err == nil {
		t.Error("128-bit entropy accepted")
	}
}

func TestRecoveryKeyDerivation(t *testing.T) {
	ent := bytes.Repeat([]byte{0x42}, 32)
	r1, err := RecoveryKey(ent)
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := RecoveryKey(ent)
	if !bytes.Equal(r1, r2) {
		t.Fatal("derivation not deterministic")
	}
	// Seed is SHA-256("sol recovery v1" || entropy), computed independently.
	const wantSeed = "95c5530d8d9487ead0db2ef6a164c54702a6a562b06e75c6b12909fbb33b3f6d"
	if got := hex.EncodeToString(r1.Seed()); got != wantSeed {
		t.Fatalf("seed %s, want %s", got, wantSeed)
	}
	other, _ := RecoveryKey(bytes.Repeat([]byte{0x43}, 32))
	if bytes.Equal(r1, other) {
		t.Fatal("different entropy, same key")
	}
}
