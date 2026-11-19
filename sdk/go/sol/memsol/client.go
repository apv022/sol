package memsol

import (
	"context"
	"crypto/rand"
	"encoding/hex"

	"github.com/apv022/sol/sdk/go/sol"
)

// Client is the sol.API for one (person, device, app) of a Network.
type Client struct {
	n      *Network
	person *Person
	device *Device
	app    string
	agent  string
}

var _ sol.API = (*Client)(nil)

// Client returns the API as seen by the person's first device running app.
func (p *Person) Client(app string) *Client { return p.Devices[0].Client(app) }

// Client returns the API as seen by this device running app.
func (d *Device) Client(app string) *Client {
	return &Client{n: d.p.n, person: d.p, device: d, app: app}
}

// AsAgent returns a copy of the client that acts as the agent key id: its calls carry
// Sol-Agent, which is what the "agents" approval policy keys on.
func (c *Client) AsAgent(id string) *Client {
	cp := *c
	cp.agent = id
	return &cp
}

// Token registers the client with its network and returns the bearer token for Server.
func (c *Client) Token() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	t := hex.EncodeToString(b)
	c.n.mu.Lock()
	c.n.tokens[t] = c
	c.n.mu.Unlock()
	return t
}

// WhoAmI implements sol.API.
func (c *Client) WhoAmI(context.Context) (sol.WhoAmI, error) {
	c.n.mu.Lock()
	defer c.n.mu.Unlock()
	scopes := []string{"feeds", "blobs", "people", "dial", "actions"}
	if r := c.person.apps[c.app]; r != nil && r.manifest != nil {
		scopes = r.manifest.Scopes
	}
	return sol.WhoAmI{
		Person: sol.PersonInfo{ID: c.person.ID, Name: c.person.Name},
		Device: sol.Device{ID: c.device.ID, Name: c.device.Name, Online: c.person.online, Anchor: c.device.Anchor},
		App:    sol.AppInfo{ID: c.app, Scopes: scopes},
	}, nil
}

// People implements sol.API.
func (c *Client) People(context.Context) ([]sol.Contact, error) {
	c.n.mu.Lock()
	defer c.n.mu.Unlock()
	out := []sol.Contact{}
	for _, p := range c.n.order {
		if c.person.contacts[p.ID] {
			out = append(out, c.person.contactView(p, c.person))
		}
	}
	return out, nil
}

// Devices implements sol.API.
func (c *Client) Devices(context.Context) ([]sol.Device, error) {
	c.n.mu.Lock()
	defer c.n.mu.Unlock()
	out := []sol.Device{}
	for _, d := range c.person.Devices {
		out = append(out, sol.Device{ID: d.ID, Name: d.Name, Online: c.person.online, Anchor: d.Anchor})
	}
	return out, nil
}
