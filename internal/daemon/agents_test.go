package daemon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/sdk/go/sol"
)

func testDaemon(t *testing.T) *Daemon {
	t.Helper()
	return testDaemonAs(t, func(_, _ netip.AddrPort) (bool, error) { return false, nil }) // login-code tests need a signed-out browser
}

// testDaemonAs is testDaemon with same decided by same (the browser's OS user).
func testDaemonAs(t *testing.T, same SameUserFunc) *Daemon {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{UI: "127.0.0.1:0", Files: filepath.Join(dir, "files"), Transports: []TransportConfig{{Name: "clearnet", Listen: "127.0.0.1:0"}}}
	if err := Init(dir, InitOptions{Name: "A", Device: "d", Config: cfg}); err != nil {
		t.Fatal(err)
	}
	d, err := Start(Options{Dir: dir, InProcessApps: true, APIOverTCP: true, Log: slog.New(slog.DiscardHandler),
		SameUser: same})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// TestAgentRenewal: a certificate with less than a third of its life left is renewed on use,
// and the client stores the new one in its key file.
func TestAgentRenewal(t *testing.T) {
	d := testDaemon(t)
	k, pub, _ := NewAgentKey("r")
	c, err := d.Keyring.IssueAgent(pub, "r", []string{"*"}, 3*time.Hour, time.Now().Add(-150*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.agents.put(infoOf(c)); err != nil {
		t.Fatal(err)
	}
	k.Cert = c.Marshal()
	p := filepath.Join(t.TempDir(), "r.key")
	_ = k.Save(p)
	cl, err := ConnectAgent(d.Dir(), p)
	if err != nil {
		t.Fatal(err)
	}
	var who AgentWhoAmI
	if err := cl.Do(context.Background(), "GET", "/agent/v1/whoami", nil, &who); err != nil {
		t.Fatal(err)
	}
	k2, _ := LoadAgentKey(p)
	nc, err := identity.ParseAgentCert(k2.Cert)
	if err != nil || nc.Expires <= c.Expires || nc.Expires-nc.Issued != 3*3600 {
		t.Fatalf("not renewed: %v %d %d", err, nc.Expires, c.Expires)
	}
	if info, _ := d.agents.find("r"); !bytes.Equal(info.Cert, k2.Cert) {
		t.Fatal("registry not updated")
	}
}

// TestAgentReplay: the same signed request is accepted once.
func TestAgentReplay(t *testing.T) {
	d := testDaemon(t)
	k, pub, _ := NewAgentKey("x")
	info, err := d.IssueAgent("x", pub, []string{"*"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	k.Cert = info.Cert
	c, err := dialAPI(d.APIURL())
	if err != nil {
		t.Fatal(err)
	}
	r, _ := http.NewRequest("GET", c.base+"/agent/v1/whoami", nil)
	k.sign(r, nil, time.Now())
	for i, want := range []int{200, 401} {
		r2 := r.Clone(context.Background())
		resp, err := c.hc.Do(r2)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("request %d: %d", i, resp.StatusCode)
		}
	}
	// A stale time is refused too.
	r3, _ := http.NewRequest("GET", c.base+"/agent/v1/whoami", nil)
	k.sign(r3, nil, time.Now().Add(-10*time.Minute))
	resp, _ := c.hc.Do(r3)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("stale: %d", resp.StatusCode)
	}
	if _, err := d.IssueAgent("x", pub, []string{"*"}, 0); !errors.Is(err, sol.ErrConflict) {
		t.Fatalf("duplicate name: %v", err)
	}
}
