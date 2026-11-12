package node

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/internal/events"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/record"
	"github.com/apv022/sol/internal/site"
)

// A site is a service whose content is a folder the node serves itself
// (config.Service.Dir, `sol site add`). It has no local port: peers reach it
// over their verified stream, and a public one is served straight on its
// own publication, so a private site is never exposed on a loopback port
// where other local programs or web pages could read it.

// siteServer is one open site folder.
type siteServer struct {
	handler http.Handler
	close   func()
}

// openSite starts serving svc.Dir, replacing an earlier server for the same
// service. It fails if the folder can't be read.
func (n *Node) openSite(svc config.Service) error {
	h, closer, err := site.Handler(svc.Dir)
	if err != nil {
		if u := errors.Unwrap(err); u != nil {
			err = u // the bare reason; the path is already in our message
		}
		return fmt.Errorf("site %q: can't read the folder %s: %w", svc.Name, svc.Dir, err)
	}
	n.mu.Lock()
	old := n.sites[svc.Name]
	n.sites[svc.Name] = &siteServer{handler: h, close: func() { closer.Close() }}
	n.mu.Unlock()
	if old != nil {
		old.close()
	}
	return nil
}

// closeSite stops serving a site. Unknown names are fine.
func (n *Node) closeSite(name string) {
	n.mu.Lock()
	s := n.sites[name]
	delete(n.sites, name)
	n.mu.Unlock()
	if s != nil {
		s.close()
	}
}

// siteHandler returns the handler for a site, or nil if its folder could
// not be opened.
func (n *Node) siteHandler(name string) http.Handler {
	n.mu.Lock()
	defer n.mu.Unlock()
	if s := n.sites[name]; s != nil {
		return s.handler
	}
	return nil
}

// openSavedSites opens every site in the config at start-up. A folder that
// is gone is logged and the service shows as not running.
func (n *Node) openSavedSites() {
	for _, svc := range n.cfg.ServiceList() {
		if svc.Dir == "" {
			continue
		}
		if err := n.openSite(svc); err != nil {
			n.log.Add(events.KindService, err.Error())
		}
	}
}

// serviceKey loads (or creates) the signing key of one public service. It
// is separate from the node key on purpose: a public service must not be
// linkable to the node. The key outlives the service, like its onion key,
// so removing and re-adding a service keeps its identity.
func (n *Node) serviceKey(name string) (*identity.Identity, error) {
	return identity.LoadOrCreate(filepath.Join(n.opts.DataDir, "services", name+".key"))
}

// signRecord builds the signed record for a public service at addrs.
func (n *Node) signRecord(name string, addrs map[string]string) ([]byte, error) {
	key, err := n.serviceKey(name)
	if err != nil {
		return nil, err
	}
	return record.Sign(key, name, addrs)
}
