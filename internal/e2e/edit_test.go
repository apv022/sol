package e2e

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/internal/control"
	"github.com/apv022/sol/internal/peers"
)

func ptr[T any](v T) *T { return &v }

// localApp starts an HTTP app answering reply and returns its port.
func localApp(t *testing.T, reply string) int {
	t.Helper()
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, reply) }))
	t.Cleanup(app.Close)
	return app.Listener.Addr().(*net.TCPAddr).Port
}

func serviceAddr(n *testNode, name string) string {
	for _, s := range n.Services() {
		if s.Name == name {
			return s.Addresses["clearnet"]
		}
	}
	return ""
}

// Editing a service's port: the peer's next connection reaches the new
// port, with no re-pairing and no change to the share.
func TestEditServicePortPeerReconnects(t *testing.T) {
	a, b := newNode(t, "alpha"), newNode(t, "beta")
	bOnA, aOnB := pair(t, a, b)
	oldPort, newPort := localApp(t, "old app"), localApp(t, "new app")
	if err := a.AddService(config.Service{Name: "web", LocalPort: oldPort, Visibility: config.VisibilityPrivate, Protocol: config.ProtocolHTTP}); err != nil {
		t.Fatal(err)
	}
	a.SetShare(bOnA.ID, "web", true)
	open := func() string {
		t.Helper()
		conn, err := b.DialPeer(b.ctx, aOnB.ID, "web")
		if err != nil {
			t.Fatal(err)
		}
		body, err := httpOver(conn, "/")
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	if got := open(); got != "old app" {
		t.Fatalf("before: %q", got)
	}
	if _, err := a.EditService("web", config.ServiceEdit{LocalPort: ptr(0)}); err == nil {
		t.Fatal("port 0 accepted")
	}
	if _, err := a.EditService("web", config.ServiceEdit{LocalPort: ptr(newPort)}); err != nil {
		t.Fatal(err)
	}
	if got := open(); got != "new app" {
		t.Fatalf("after the port change: %q", got)
	}
}

// Renaming carries the peer's share to the new name; the old name is gone.
func TestEditServiceRenameKeepsShare(t *testing.T) {
	a, b := newNode(t, "alpha"), newNode(t, "beta")
	bOnA, aOnB := pair(t, a, b)
	if err := a.AddService(config.Service{Name: "web", LocalPort: localApp(t, "hi"), Visibility: config.VisibilityPrivate, Protocol: config.ProtocolHTTP}); err != nil {
		t.Fatal(err)
	}
	a.SetShare(bOnA.ID, "web", true)
	live, err := b.DialPeer(b.ctx, aOnB.ID, "web")
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if _, err := a.EditService("web", config.ServiceEdit{Name: ptr("site")}); err != nil {
		t.Fatal(err)
	}
	for _, p := range a.Peers() {
		if p.ID == bOnA.ID && (!slices.Contains(p.Shares, "site") || slices.Contains(p.Shares, "web")) {
			t.Fatalf("shares after rename: %v", p.Shares)
		}
	}
	// The live stream is untouched by the rename.
	if body, err := httpOver(live, "/"); err != nil || body != "hi" {
		t.Fatalf("live stream after rename: %q %v", body, err)
	}
	p, err := b.RefreshPeer(b.ctx, aOnB.ID)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range p.Services {
		names = append(names, s.Name)
	}
	if !slices.Contains(names, "site") || slices.Contains(names, "web") {
		t.Fatalf("b sees %v after refresh", names)
	}
	if _, err := b.DialPeer(b.ctx, aOnB.ID, "web"); err == nil {
		t.Fatal("old name still opens")
	}
	if c, err := b.DialPeer(b.ctx, aOnB.ID, "site"); err != nil {
		t.Fatalf("new name: %v", err)
	} else {
		c.Close()
	}
	// Renaming onto a taken or built-in name is refused.
	a.AddService(config.Service{Name: "db", LocalPort: 5432, Visibility: config.VisibilityPrivate})
	for _, bad := range []string{"db", peers.Files} {
		if _, err := a.EditService("site", config.ServiceEdit{Name: ptr(bad)}); err == nil {
			t.Fatalf("renamed onto %q", bad)
		}
	}
}

// Visibility through Edit publishes and unpublishes; a public service that
// is renamed or gets a new port stays published.
func TestEditServiceVisibilityPublishes(t *testing.T) {
	a := newNode(t, "alpha")
	if err := a.AddService(config.Service{Name: "blog", LocalPort: localApp(t, "v1"), Visibility: config.VisibilityPrivate, Protocol: config.ProtocolHTTP}); err != nil {
		t.Fatal(err)
	}
	if serviceAddr(a, "blog") != "" {
		t.Fatal("private service published")
	}
	if _, err := a.EditService("blog", config.ServiceEdit{Visibility: ptr(config.VisibilityPublic)}); err != nil {
		t.Fatal(err)
	}
	addr := serviceAddr(a, "blog")
	if body, err := get("http://" + addr + "/"); addr == "" || err != nil || body != "v1" {
		t.Fatalf("public after edit: %q %q %v", addr, body, err)
	}
	if _, err := a.EditService("blog", config.ServiceEdit{Name: ptr("news"), LocalPort: ptr(localApp(t, "v2"))}); err != nil {
		t.Fatal(err)
	}
	addr = serviceAddr(a, "news")
	if body, err := get("http://" + addr + "/"); addr == "" || err != nil || body != "v2" {
		t.Fatalf("after rename and port change: %q %q %v", addr, body, err)
	}
	if _, err := a.EditService("news", config.ServiceEdit{Visibility: ptr(config.VisibilityPrivate)}); err != nil {
		t.Fatal(err)
	}
	if serviceAddr(a, "news") != "" {
		t.Fatal("still has a public address")
	}
	if _, err := get("http://" + addr + "/"); err == nil {
		t.Fatal("unpublished service still reachable")
	}
}

// The owner deletes a shared file: a peer's next look at _files no longer
// lists or serves it; a rename shows at once too.
func TestDeleteSharedFilePeerNoLongerSees(t *testing.T) {
	a, b := newNode(t, "alpha"), newNode(t, "beta")
	bOnA, aOnB := pair(t, a, b)
	a.SetShare(bOnA.ID, peers.Files, true)
	hash, err := a.AddFile(strings.NewReader("holiday photos"), "photos.zip", false)
	if err != nil {
		t.Fatal(err)
	}
	page := func(path string) string {
		t.Helper()
		conn, err := b.DialPeer(b.ctx, aOnB.ID, peers.Files)
		if err != nil {
			t.Fatal(err)
		}
		body, err := httpOver(conn, path)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	if !strings.Contains(page("/"), "photos.zip") || page("/blob/"+hash) != "holiday photos" {
		t.Fatal("peer can't see the shared file")
	}
	if err := a.RenameFile("photos.zip", "trip.zip"); err != nil {
		t.Fatal(err)
	}
	if p := page("/"); !strings.Contains(p, "trip.zip") || strings.Contains(p, "photos.zip") {
		t.Fatal("rename not visible to the peer")
	}
	if err := a.DeleteFile("trip.zip"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page("/"), "trip.zip") {
		t.Fatal("deleted file still listed for the peer")
	}
	if strings.Contains(page("/blob/"+hash), "holiday photos") {
		t.Fatal("deleted file still served to the peer")
	}
	if a.Blobs().Has(hash) {
		t.Fatal("bytes of a deleted file left on disk")
	}
}

// `sol service edit` and `sol files` go through the control socket.
func TestEditAndFilesOverControlSocket(t *testing.T) {
	a := newNode(t, "alpha")
	ctx := context.Background()
	call := func(req control.Request, out any) error { return control.Call(ctx, a.ControlPath(), req, out) }
	if err := call(control.Request{Op: control.OpServiceAdd, Service: "web", Port: 8000}, nil); err != nil {
		t.Fatal(err)
	}
	var s control.LocalService
	err := call(control.Request{Op: control.OpServiceEdit, Service: "web",
		Edit: &control.ServiceEdit{Name: ptr("app"), Port: ptr(8001), Protocol: ptr(config.ProtocolHTTP)}}, &s)
	if err != nil || s.Name != "app" || s.Port != 8001 || s.Protocol != config.ProtocolHTTP {
		t.Fatalf("edit: %+v %v", s, err)
	}
	if err := call(control.Request{Op: control.OpServiceEdit, Service: "app", Edit: &control.ServiceEdit{Port: ptr(-1)}}, nil); err == nil {
		t.Fatal("bad port accepted")
	}

	// file-add streams like send.
	c, err := control.Dial(ctx, a.ControlPath())
	if err != nil {
		t.Fatal(err)
	}
	c.Send(control.Request{Op: control.OpFileAdd, Name: "notes.txt"})
	if err := c.Recv(nil); err != nil {
		t.Fatal(err)
	}
	io.WriteString(c.Conn, "some notes")
	c.CloseWrite()
	var sent control.Sent
	if err := c.Recv(&sent); err != nil || sent.Size != int64(len("some notes")) {
		t.Fatalf("file-add: %+v %v", sent, err)
	}
	c.Close()
	if err := call(control.Request{Op: control.OpFileRename, Name: "notes.txt", Rename: "todo.txt"}, nil); err != nil {
		t.Fatal(err)
	}
	var files []control.File
	if err := call(control.Request{Op: control.OpFiles}, &files); err != nil || len(files) != 1 || files[0].Name != "todo.txt" || files[0].Hash != sent.Hash {
		t.Fatalf("files: %+v %v", files, err)
	}
	if err := call(control.Request{Op: control.OpFileRemove, Name: "todo.txt"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := call(control.Request{Op: control.OpFileRemove, Name: "todo.txt"}, nil); err == nil {
		t.Fatal("removing a missing file succeeded")
	}
}
