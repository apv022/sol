package memsol

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"

	"github.com/apv022/sol/sdk/go/sol"
)

type feedState struct {
	entries []sol.Entry // pos = index+1
	last    map[string]sol.Entry
}

// readable reports whether reader may read the feed at addr: it owns it, it is shared with the
// reader, or the reader share-wrote a same-named feed to the owner (co-author feed). n.mu held.
func (n *Network) readable(reader, addr string) bool {
	owner, app, name, err := sol.ParseAddress(addr)
	if err != nil {
		return false
	}
	if owner == reader {
		return true
	}
	if _, ok := n.shares[addr][reader]; ok {
		return true
	}
	return n.shares[sol.Address(reader, app, name)][owner] == sol.ModeWrite
}

// Append implements sol.API.
func (c *Client) Append(_ context.Context, feed string, body any) (sol.Entry, error) {
	if !sol.ValidFeedName(feed) {
		return sol.Entry{}, sol.Errorf("bad_request", "bad feed name")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return sol.Entry{}, sol.Errorf("bad_request", err.Error())
	}
	if len(raw) > sol.MaxBodySize {
		return sol.Entry{}, sol.Errorf("bad_request", "body over 64 KiB: put it in a blob")
	}
	n := c.n
	n.mu.Lock()
	defer n.mu.Unlock()
	addr := sol.Address(c.person.ID, c.app, feed)
	fs := n.feeds[addr]
	if fs == nil {
		fs = &feedState{last: map[string]sol.Entry{}}
		n.feeds[addr] = fs
	}
	prev := fs.last[c.device.ID]
	e := sol.Entry{Feed: addr, Seq: prev.Seq + 1, Pos: uint64(len(fs.entries)) + 1, Device: c.device.ID,
		Time: n.Now().UTC(), Body: raw, Prev: prev.Hash}
	if e.Hash, err = e.ComputeHash(); err != nil {
		return sol.Entry{}, sol.Errorf("bad_request", err.Error())
	}
	fs.entries = append(fs.entries, e)
	fs.last[c.device.ID] = e
	n.append(logged{kind: sol.EventEntry, app: c.app, entry: &e})
	return e, nil
}

// Read implements sol.API.
func (c *Client) Read(_ context.Context, address string, from uint64, limit int) ([]sol.Entry, error) {
	_, app, _, err := sol.ParseAddress(address)
	if err != nil {
		return nil, err
	}
	if app != c.app {
		return nil, sol.Errorf("forbidden", "an app reads only its own feeds")
	}
	if limit <= 0 {
		limit = 500
	}
	limit = min(limit, 5000)
	if from < 1 {
		from = 1
	}
	n := c.n
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.readable(c.person.ID, address) {
		return nil, sol.Errorf("forbidden", "feed is not shared with you")
	}
	out := []sol.Entry{}
	if fs := n.feeds[address]; fs != nil && from <= uint64(len(fs.entries)) {
		out = append(out, fs.entries[from-1:min(uint64(len(fs.entries)), from-1+uint64(limit))]...)
	}
	return out, nil
}

// Feeds implements sol.API.
func (c *Client) Feeds(_ context.Context, owner string) ([]sol.FeedInfo, error) {
	n := c.n
	n.mu.Lock()
	defer n.mu.Unlock()
	out := []sol.FeedInfo{}
	for addr, fs := range n.feeds {
		o, app, _, _ := sol.ParseAddress(addr)
		if app != c.app || (owner != "" && o != owner) || !n.readable(c.person.ID, addr) {
			continue
		}
		out = append(out, sol.FeedInfo{Feed: addr, Pos: uint64(len(fs.entries))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Feed < out[j].Feed })
	return out, nil
}

func containsHash(body json.RawMessage, hash string) bool {
	return bytes.Contains(body, []byte(hash))
}

// Share implements sol.API.
func (c *Client) Share(_ context.Context, req sol.ShareRequest) error {
	return c.setShare(req, true)
}

// Unshare implements sol.API.
func (c *Client) Unshare(_ context.Context, req sol.ShareRequest) error {
	return c.setShare(req, false)
}

func (c *Client) setShare(req sol.ShareRequest, on bool) error {
	if !sol.ValidFeedName(req.Feed) {
		return sol.Errorf("bad_request", "bad feed name")
	}
	mode := req.Mode
	if mode == "" {
		mode = sol.ModeRead
	}
	if mode != sol.ModeRead && mode != sol.ModeWrite {
		return sol.Errorf("bad_request", "mode must be read or write")
	}
	n := c.n
	n.mu.Lock()
	defer n.mu.Unlock()
	addr := sol.Address(c.person.ID, c.app, req.Feed)
	for _, id := range req.People {
		t := n.people[id]
		if t == nil || id == c.person.ID || !c.person.contacts[id] {
			return sol.Errorf("not_found", "unknown person "+id)
		}
	}
	for _, id := range req.People {
		t := n.people[id]
		theirs := sol.Address(id, c.app, req.Feed)
		wasMine := n.readable(id, addr)
		wasTheirs := n.readable(c.person.ID, theirs)
		if on {
			if n.shares[addr] == nil {
				n.shares[addr] = map[string]string{}
			}
			n.shares[addr][id] = mode
		} else if n.shares[addr] != nil {
			delete(n.shares[addr], id)
			if len(n.shares[addr]) == 0 {
				delete(n.shares, addr)
			}
		}
		if now := n.readable(id, addr); now != wasMine {
			n.append(logged{kind: sol.EventFeed, app: c.app, target: id, feed: &sol.FeedChange{Feed: addr, Readable: now}})
		}
		if now := n.readable(c.person.ID, theirs); now != wasTheirs {
			n.append(logged{kind: sol.EventFeed, app: c.app, target: c.person.ID, feed: &sol.FeedChange{Feed: theirs, Readable: now}})
		}
		n.emitPerson(t, c.person)
		n.emitPerson(c.person, t)
	}
	return nil
}

// Shares implements sol.API.
func (c *Client) Shares(_ context.Context, feed string) ([]sol.Share, error) {
	n := c.n
	n.mu.Lock()
	defer n.mu.Unlock()
	m := n.shares[sol.Address(c.person.ID, c.app, feed)]
	out := []sol.Share{}
	for p, mode := range m {
		out = append(out, sol.Share{Feed: feed, Person: p, Mode: mode})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Person < out[j].Person })
	return out, nil
}
