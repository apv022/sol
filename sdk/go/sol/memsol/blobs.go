package memsol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"

	"github.com/apv022/sol/sdk/go/sol"
)

// PutBlob implements sol.API.
func (c *Client) PutBlob(_ context.Context, r io.Reader, _ string) (sol.BlobInfo, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return sol.BlobInfo{}, err
	}
	sum := sha256.Sum256(data)
	h := "sha256:" + hex.EncodeToString(sum[:])
	c.n.mu.Lock()
	defer c.n.mu.Unlock()
	c.n.blobs[h] = data
	if c.n.holders[h] == nil {
		c.n.holders[h] = map[string]bool{}
	}
	c.n.holders[h][c.person.ID] = true
	return sol.BlobInfo{Hash: h, Size: int64(len(data))}, nil
}

// locate decides whether the client's person can get the blob, and from whom. n.mu held.
func (c *Client) locate(hash, from string) (data []byte, local bool, err error) {
	n := c.n
	data, ok := n.blobs[hash]
	if !ok {
		return nil, false, sol.Errorf("not_found", "no such blob")
	}
	me := c.person.ID
	if n.holders[hash][me] {
		return data, true, nil
	}
	sawOffline := false
	for _, h := range n.order {
		if !n.holders[hash][h.ID] || (from != "" && h.ID != from) || !n.paired(me, h.ID) {
			continue
		}
		// API.md rule: referenced by an entry of a feed h shares with me.
		for addr, fs := range n.feeds {
			if o, _, _, _ := sol.ParseAddress(addr); o != h.ID || !n.readable(me, addr) {
				continue
			}
			for _, e := range fs.entries {
				if containsHash(e.Body, hash) {
					if !h.online {
						sawOffline = true
						goto next
					}
					return data, false, nil
				}
			}
		}
	next:
	}
	if sawOffline {
		return nil, false, sol.Errorf("unavailable", "the holder is offline")
	}
	return nil, false, sol.Errorf("not_found", "no such blob")
}

// GetBlob implements sol.API. Fetching a non-local blob caches it for the caller's person.
func (c *Client) GetBlob(_ context.Context, hash string, o sol.BlobOptions) (io.ReadCloser, error) {
	c.n.mu.Lock()
	data, _, err := c.locate(hash, o.From)
	if err == nil {
		c.n.holders[hash][c.person.ID] = true
	}
	c.n.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if o.Offset < 0 || o.Offset > int64(len(data)) {
		return nil, sol.Errorf("bad_request", "range out of bounds")
	}
	data = data[o.Offset:]
	if o.Length > 0 && o.Length < int64(len(data)) {
		data = data[:o.Length]
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// StatBlob implements sol.API. It does not cache the blob.
func (c *Client) StatBlob(_ context.Context, hash string, o sol.BlobOptions) (sol.BlobStat, error) {
	c.n.mu.Lock()
	defer c.n.mu.Unlock()
	data, local, err := c.locate(hash, o.From)
	if err != nil {
		return sol.BlobStat{}, err
	}
	return sol.BlobStat{Size: int64(len(data)), Local: local}, nil
}

// Pin implements sol.API. Only blobs the person holds can be pinned.
func (c *Client) Pin(_ context.Context, hashes []string) error {
	c.n.mu.Lock()
	defer c.n.mu.Unlock()
	for _, h := range hashes {
		if !c.n.holders[h][c.person.ID] {
			return sol.Errorf("not_found", "blob not local: "+h)
		}
	}
	for _, h := range hashes {
		c.person.pinned[h] = true
	}
	return nil
}

// Unpin implements sol.API.
func (c *Client) Unpin(_ context.Context, hashes []string) error {
	c.n.mu.Lock()
	defer c.n.mu.Unlock()
	for _, h := range hashes {
		delete(c.person.pinned, h)
	}
	return nil
}
