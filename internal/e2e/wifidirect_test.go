package e2e

import (
	"bytes"
	"errors"
	"net/netip"
	"testing"

	sold "github.com/apv022/sol/internal/daemon"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/nearby"
	"github.com/apv022/sol/sdk/go/sol"
)

// TestWifiDirectDecision: two paired phones that lost each other (restarted on new addresses,
// no shared LAN) and run under the Android shell. Each advertises an ad only the other
// recognises; the one with the lower device key is told to lead and gets group credentials,
// the other waits, then joins with exactly those credentials once the leader's ad carries
// them. A stranger's ad matches nothing. Without the shell the switch is not_implemented.
func TestWifiDirectDecision(t *testing.T) {
	if _, err := (&sold.Daemon{}).SetWifiDirect(true); !isCode(err, "not_implemented") {
		t.Fatalf("without the shell: %v", err)
	}
	t.Setenv("SOL_ANDROID_SHELL", "1")
	busA, busB := nearby.NewBus(), nearby.NewBus() // no LAN in common
	a, _ := nearbyStart(t, "", sold.InitOptions{Name: "Ana", Device: "phone"}, busA)
	b, _ := nearbyStart(t, "", sold.InitOptions{Name: "Ben", Device: "phone"}, busB)
	pairNodes(t, a, b)
	eventually(t, "paired phones connected", func() bool { return connected(a, b) && connected(b, a) })
	dirA, dirB := a.Dir(), b.Dir()
	a.Close()
	b.Close()
	a, _ = nearbyStart(t, dirA, sold.InitOptions{}, busA)
	b, _ = nearbyStart(t, dirB, sold.InitOptions{}, busB)
	if connected(a, b) {
		t.Fatal("connected without a path")
	}

	// Off by default: no plan.
	adB, err := b.WifiDirectAdFor(nil)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := a.WifiDirectMatch("none", []nearby.AdSeen{{ID: "b", TXT: adB.TXT}}); err != nil || p.Role != "none" {
		t.Fatalf("while off: %+v %v", p, err)
	}
	for _, d := range []*sold.Daemon{a, b} {
		st, err := d.SetWifiDirect(true)
		if err != nil || st.WifiDirect == nil || !st.WifiDirect.Enabled {
			t.Fatalf("SetWifiDirect: %+v %v", st, err)
		}
	}
	lo, hi := a, b
	if bytes.Compare(dev(t, a), dev(t, b)) > 0 {
		lo, hi = b, a
	}
	adLo, _ := lo.WifiDirectAdFor(nil)
	adHi, _ := hi.WifiDirectAdFor(nil)
	lead, err := lo.WifiDirectMatch("none", []nearby.AdSeen{{ID: "hi", TXT: adHi.TXT}})
	if err != nil || lead.Role != "lead" || lead.Group == nil {
		t.Fatalf("lower key: %+v %v", lead, err)
	}
	if p, _ := hi.WifiDirectMatch("none", []nearby.AdSeen{{ID: "lo", TXT: adLo.TXT}}); p.Role != "wait" {
		t.Fatalf("higher key before the group: %+v", p)
	}
	adLo, err = lo.WifiDirectAdFor(lead.Group)
	if err != nil {
		t.Fatal(err)
	}
	join, _ := hi.WifiDirectMatch("none", []nearby.AdSeen{{ID: "lo", TXT: adLo.TXT}})
	if join.Role != "join" || join.ID != "lo" || join.Group == nil || *join.Group != *lead.Group {
		t.Fatalf("higher key with the group: %+v", join)
	}
	if p, _ := lo.WifiDirectMatch("owner", []nearby.AdSeen{{ID: "hi", TXT: adHi.TXT}}); p.Role != "stay" {
		t.Fatalf("owner: %+v", p)
	}

	// A stranger (paired with nobody here) is not recognised.
	s, _ := nearbyStart(t, "", sold.InitOptions{Name: "Sam", Device: "phone"}, nearby.NewBus())
	s.SetWifiDirect(true)
	adS, _ := s.WifiDirectAdFor(nil)
	if p, _ := lo.WifiDirectMatch("none", []nearby.AdSeen{{ID: "s", TXT: adS.TXT}}); p.Role != "none" || len(p.Matched) != 0 {
		t.Fatalf("stranger: %+v", p)
	}

	// The shell reports its state and the group's address; both show in the status.
	if err := hi.ReportWifiDirect("connected", "in a group with 1 device"); err != nil {
		t.Fatal(err)
	}
	if err := hi.SetNearbyInterfaces([]netip.Addr{netip.MustParseAddr("192.168.49.23")}); err != nil {
		t.Fatal(err)
	}
	if err := hi.SetNearbyInterfaces([]netip.Addr{netip.MustParseAddr("239.1.1.1")}); !isCode(err, "bad_request") {
		t.Fatalf("multicast address accepted: %v", err)
	}
	if err := hi.ReportWifiDirect("dancing", ""); !isCode(err, "bad_request") {
		t.Fatalf("unknown state accepted: %v", err)
	}
	st := hi.Nearby()
	if st.WifiDirect == nil || st.WifiDirect.State != "connected" {
		t.Fatalf("status: %+v", st.WifiDirect)
	}
	row := hi.Transports()[len(hi.Transports())-1]
	if row.Name != "nearby" || !bytes.Contains([]byte(row.Detail), []byte("Wi-Fi Direct connected")) {
		t.Fatalf("row: %+v", row)
	}
	if st, _ := hi.SetWifiDirect(false); st.WifiDirect.Enabled || st.WifiDirect.State != "off" {
		t.Fatalf("off: %+v", st.WifiDirect)
	}
	if cfg, _ := sold.LoadConfig(hi.Dir()); cfg.WifiDirect == nil || *cfg.WifiDirect {
		t.Fatalf("config: %+v", cfg.WifiDirect)
	}

}

func dev(t *testing.T, d *sold.Daemon) []byte {
	t.Helper()
	k, err := identity.ParseID(d.DeviceID())
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func isCode(err error, code string) bool {
	var se *sol.Error
	return errors.As(err, &se) && se.Code == code
}
