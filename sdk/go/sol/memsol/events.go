package memsol

import (
	"context"
	"strconv"

	"github.com/apv022/sol/sdk/go/sol"
)

// logged is one record of the network-wide event log. The event id is its 1-based index, so
// Last-Event-ID resume is a cursor into the log.
type logged struct {
	kind   string
	app    string // only the app's own subscribers see it
	target string // person for feed/person/inbox events
	entry  *sol.Entry
	feed   *sol.FeedChange
	person *sol.Contact
	inbox  *sol.InboxEvent
}

// append adds to the log and wakes subscribers. n.mu held.
func (n *Network) append(l logged) {
	n.log = append(n.log, l)
	close(n.notify)
	n.notify = make(chan struct{})
}

// emitPerson tells viewer that other changed. The event is delivered to every app of viewer
// that subscribed to people events. n.mu held.
func (n *Network) emitPerson(viewer, other *Person) {
	cv := viewer.contactView(other, viewer)
	n.append(logged{kind: sol.EventPerson, target: viewer.ID, person: &cv})
}

// Events implements sol.API. There is no replay unless opts.LastEventID is set: a new
// subscription starts at "now".
func (c *Client) Events(ctx context.Context, opts sol.EventOptions) (<-chan sol.Event, error) {
	n := c.n
	n.mu.Lock()
	cursor := len(n.log)
	if opts.LastEventID != "" {
		id, err := strconv.Atoi(opts.LastEventID)
		if err != nil || id < 0 || id > len(n.log) {
			n.mu.Unlock()
			return nil, sol.Errorf("bad_request", "bad Last-Event-ID")
		}
		cursor = id
	}
	n.mu.Unlock()
	subscribed := map[string]bool{}
	for _, f := range opts.Feeds {
		subscribed[f] = true
	}
	out := make(chan sol.Event)
	go func() {
		defer close(out)
		for {
			n.mu.Lock()
			var batch []sol.Event
			for ; cursor < len(n.log); cursor++ {
				if ev, ok := c.visible(n.log[cursor], opts, subscribed); ok {
					ev.ID = strconv.Itoa(cursor + 1)
					batch = append(batch, ev)
				}
			}
			wait := n.notify
			n.mu.Unlock()
			for _, ev := range batch {
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
			select {
			case <-wait:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// visible decides whether l goes to this client's subscription. n.mu held.
func (c *Client) visible(l logged, o sol.EventOptions, subscribed map[string]bool) (sol.Event, bool) {
	switch l.kind {
	case sol.EventEntry:
		a := l.entry.Feed
		if l.app != c.app || (len(subscribed) > 0 && !subscribed[a]) || !c.n.readable(c.person.ID, a) {
			return sol.Event{}, false
		}
		e := *l.entry
		return sol.Event{Type: sol.EventEntry, Entry: &e}, true
	case sol.EventFeed:
		if l.app != c.app || l.target != c.person.ID {
			return sol.Event{}, false
		}
		f := *l.feed
		return sol.Event{Type: sol.EventFeed, Feed: &f}, true
	case sol.EventPerson:
		if !o.People || l.target != c.person.ID {
			return sol.Event{}, false
		}
		p := *l.person
		return sol.Event{Type: sol.EventPerson, Person: &p}, true
	case sol.EventInbox:
		if !o.Inbox || l.app != c.app || l.target != c.person.ID {
			return sol.Event{}, false
		}
		i := *l.inbox
		return sol.Event{Type: sol.EventInbox, Inbox: &i}, true
	}
	return sol.Event{}, false
}
