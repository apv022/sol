package sol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// canonical re-encodes JSON with sorted object keys and no insignificant whitespace.
func canonical(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage("null"), nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(v) // maps marshal with sorted keys
}

// ComputeHash returns the entry hash: sha256 over a canonical JSON encoding of every field
// except the hash itself (the fake in memsol has no signatures; a real daemon signs this too).
func (e Entry) ComputeHash() (string, error) {
	body, err := canonical(e.Body)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(struct {
		Feed   string          `json:"feed"`
		Seq    uint64          `json:"seq"`
		Device string          `json:"device"`
		Time   string          `json:"time"`
		Body   json.RawMessage `json:"body"`
		Prev   string          `json:"prev"`
	}{e.Feed, e.Seq, e.Device, e.Time.UTC().Format(time.RFC3339Nano), body, e.Prev})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// VerifyChain checks hashes and, per device, that seq runs 1,2,3... with prev linking to the
// previous entry of the same device. entries may be interleaved devices in pos order; the first
// entry seen for a device is only chain-checked when its seq is 1.
func VerifyChain(entries []Entry) error {
	last := map[string]Entry{}
	for _, e := range entries {
		h, err := e.ComputeHash()
		if err != nil {
			return err
		}
		if h != e.Hash {
			return Errorf("conflict", "entry hash mismatch at seq "+itoa(e.Seq))
		}
		p, seen := last[e.Device]
		switch {
		case seen && (e.Seq != p.Seq+1 || e.Prev != p.Hash):
			return Errorf("conflict", "broken chain at seq "+itoa(e.Seq))
		case !seen && e.Seq == 1 && e.Prev != "":
			return Errorf("conflict", "first entry has a prev")
		}
		last[e.Device] = e
	}
	return nil
}

func itoa(n uint64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
