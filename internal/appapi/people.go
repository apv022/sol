package appapi

import (
	"cmp"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/apv022/sol/internal/feeds"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/sdk/go/sol"
)

// online reports whether a session with the device is up.
func (s *Server) online(dev []byte) bool {
	if s.n.Sessions == nil {
		return false
	}
	ss, ok := s.n.Sessions.Session(dev)
	return ok && ss.Alive()
}

// isContact reports whether id is a (not removed) contact.
func (s *Server) isContact(id string) bool {
	pub, err := identity.ParseID(id)
	return err == nil && s.n.Contacts != nil && s.n.Contacts.IsContact(pub)
}

// Contact returns a contact as GET /v1/people shows it.
func (s *Server) Contact(id string) (sol.Contact, bool) {
	pub, err := identity.ParseID(id)
	if err != nil || s.n.Contacts == nil || !s.n.Contacts.IsContact(pub) {
		return sol.Contact{}, false
	}
	name, _ := s.n.Contacts.Name(pub)
	c := sol.Contact{ID: id, Name: name, Petname: petname(name, id), Devices: []sol.Device{},
		SharesWithMe: []string{}, IShare: []string{}}
	if s.n.Cache != nil {
		if _, dl, ok := s.n.Cache.Latest(pub); ok {
			for _, d := range dl.Devices {
				on := s.online(d.Key)
				c.Online = c.Online || on
				c.Devices = append(c.Devices, sol.Device{ID: identity.ID(d.Key), Name: d.Name, Online: on, Anchor: d.Anchor()})
			}
		}
	}
	if s.n.Shares != nil {
		for k := range s.n.Shares.IncomingAll(id) {
			c.SharesWithMe = append(c.SharesWithMe, k)
		}
		for addr, why := range s.n.Shares.ReadableBy(id) {
			if why == feeds.ModeCoauthor {
				continue
			}
			if _, app, name, err := feeds.ParseAddress(addr); err == nil {
				c.IShare = append(c.IShare, app+"/"+name)
			}
		}
	}
	sort.Strings(c.SharesWithMe)
	sort.Strings(c.IShare)
	return c, true
}

// petname is the short handle of a contact: the lowercased first word of their name, or the
// start of their ID when they have no usable name.
func petname(name, id string) string {
	f := strings.Fields(strings.ToLower(name))
	if len(f) == 0 {
		return id[:8]
	}
	return f[0]
}

// contacts returns every contact, by name then ID.
func (s *Server) contacts() []sol.Contact {
	out := []sol.Contact{}
	if s.n.Contacts == nil {
		return out
	}
	for _, id := range s.n.Contacts.List() {
		if c, ok := s.Contact(id); ok {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b sol.Contact) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	return out
}

// people serves GET /v1/people.
func (s *Server) people(q req, w http.ResponseWriter, r *http.Request) (any, error) {
	return s.contacts(), nil
}

// devices serves GET /v1/devices: my devices, this one always online.
func (s *Server) devices(q req, w http.ResponseWriter, r *http.Request) (any, error) {
	out := []sol.Device{}
	b, err := s.n.Keyring.Bundle()
	if err != nil {
		return out, nil
	}
	mine := s.n.Keyring.DevicePublic()
	for _, d := range b.Devices.Devices {
		on := string(d.Key) == string(mine) || s.online(d.Key)
		out = append(out, sol.Device{ID: identity.ID(d.Key), Name: d.Name, Online: on, Anchor: d.Anchor()})
	}
	return out, nil
}

// PersonChanged pushes a "person" event for a contact (online state, devices or shares changed).
func (s *Server) PersonChanged(id string) {
	if c, ok := s.Contact(id); ok {
		s.events.add(rec{kind: sol.EventPerson, person: &c})
	}
}
