package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// wordsTxt is the BIP-39 English word list, vendored verbatim from
// github.com/bitcoin/bips (bip-0039/english.txt). It is data, not code.
//
//go:embed words.txt
var wordsTxt []byte

// wordsSHA256 is the SHA-256 of the canonical BIP-39 English list. init
// refuses to run with any other file: a changed list would turn everyone's
// paper backup into a different key.
const wordsSHA256 = "2f5eed53a4727b4bf8880d8f3f199efc90e58503646d9ff8eff3a2ed3b24dbda"

// EntropySize is the size of the recovery entropy: 256 bits.
const EntropySize = 32

// WordCount is the number of recovery words: (256 + 8 checksum bits) / 11.
const WordCount = 24

// recoveryContext prefixes the entropy when deriving R's seed.
const recoveryContext = "sol recovery v1"

var (
	wordList  []string
	wordIndex map[string]int
)

func init() {
	// A CRLF checkout (Windows autocrlf) must not brick Sol: hash the canonical LF form.
	wordsTxt := bytes.ReplaceAll(wordsTxt, []byte("\r"), nil)
	sum := sha256.Sum256(wordsTxt)
	if hex.EncodeToString(sum[:]) != wordsSHA256 {
		panic("identity: words.txt is not the canonical BIP-39 English list")
	}
	wordList = strings.Fields(string(wordsTxt))
	if len(wordList) != 2048 {
		panic("identity: words.txt must hold 2048 words")
	}
	wordIndex = make(map[string]int, len(wordList))
	for i, w := range wordList {
		wordIndex[w] = i
	}
}

// ErrBadWords is returned when recovery words are unknown, the wrong count,
// or fail the BIP-39 checksum. It never says which word, so it can be shown
// without echoing the backup.
var ErrBadWords = errors.New("identity: recovery words are not valid")

// EntropyToWords encodes 256-bit entropy as 24 BIP-39 English words: the
// entropy followed by the first 8 bits of its SHA-256, in 11-bit groups.
func EntropyToWords(entropy []byte) ([]string, error) {
	if len(entropy) != EntropySize {
		return nil, fmt.Errorf("identity: entropy must be %d bytes", EntropySize)
	}
	sum := sha256.Sum256(entropy)
	bits := append(append([]byte{}, entropy...), sum[0]) // 264 bits
	words := make([]string, WordCount)
	for i := range words {
		idx := 0
		for j := 0; j < 11; j++ {
			bit := i*11 + j
			idx = idx<<1 | int(bits[bit/8]>>(7-bit%8)&1)
		}
		words[i] = wordList[idx]
	}
	return words, nil
}

// WordsToEntropy decodes 24 words back into the entropy, checking the BIP-39
// checksum. Case and surrounding whitespace are ignored.
func WordsToEntropy(words []string) ([]byte, error) {
	if len(words) != WordCount {
		return nil, ErrBadWords
	}
	bits := make([]byte, EntropySize+1)
	for i, w := range words {
		idx, ok := wordIndex[strings.ToLower(strings.TrimSpace(w))]
		if !ok {
			return nil, ErrBadWords
		}
		for j := 0; j < 11; j++ {
			if idx>>(10-j)&1 == 1 {
				bit := i*11 + j
				bits[bit/8] |= 1 << (7 - bit%8)
			}
		}
	}
	entropy := bits[:EntropySize]
	if sum := sha256.Sum256(entropy); sum[0] != bits[EntropySize] {
		return nil, ErrBadWords
	}
	return entropy, nil
}

// ParseWords splits a typed or pasted phrase into words and decodes it.
func ParseWords(phrase string) ([]byte, error) { return WordsToEntropy(strings.Fields(phrase)) }

// RecoveryKey derives R from the entropy: the ed25519 seed is
// SHA-256("sol recovery v1" || entropy), a plain concatenation (no length
// prefixes; this is a key derivation, not a signed object). Its public key is
// the person ID.
func RecoveryKey(entropy []byte) (ed25519.PrivateKey, error) {
	if len(entropy) != EntropySize {
		return nil, fmt.Errorf("identity: entropy must be %d bytes", EntropySize)
	}
	h := sha256.New()
	h.Write([]byte(recoveryContext))
	h.Write(entropy)
	return ed25519.NewKeyFromSeed(h.Sum(nil)), nil
}
