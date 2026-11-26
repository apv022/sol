package e2e

import (
	"bytes"
	"io"
	"net"
	"strconv"
	"testing"

	sold "github.com/apv022/sol/internal/daemon"
	"github.com/apv022/sol/internal/nearby"
)

// TestBLELink: two paired phones that lost each other (restarted on new addresses, no LAN in
// common, no Wi-Fi Direct) under the Android shell, Bluetooth LE on. Each recognises the other's
// adverts; the lower key is told to connect (with the peer's PSM and a link handle), the higher
// one waits. The "shells" then do what Android would: the lower key's opened L2CAP channel is
// bridged "out" with the handle, the higher key's accepted one "in", and a session comes up over
// it, authenticated as usual, on the ble path. A stranger's adverts match nothing; switched off,
// the link is cut. Off the shell the switch is not_implemented.
func TestBLELink(t *testing.T) {
	if _, err := (&sold.Daemon{}).SetBLE(true); !isCode(err, "not_implemented") {
		t.Fatalf("without the shell: %v", err)
	}
	t.Setenv("SOL_ANDROID_SHELL", "1")
	busA, busB := nearby.NewBus(), nearby.NewBus()
	a, _ := nearbyStart(t, "", sold.InitOptions{Name: "Ana", Device: "phone"}, busA)
	b, _ := nearbyStart(t, "", sold.InitOptions{Name: "Ben", Device: "phone"}, busB)
	pairNodes(t, a, b)
	eventually(t, "paired phones connected", func() bool { return connected(a, b) && connected(b, a) })
	dirA, dirB := a.Dir(), b.Dir()
	a.Close()
	b.Close()
	a, _ = nearbyStart(t, dirA, sold.InitOptions{}, busA)
	b, _ = nearbyStart(t, dirB, sold.InitOptions{}, busB)

	// Off by default: nothing advertised, nothing decided.
	if ad, err := a.BLEAdFor(0x80); err != nil || len(ad.Adverts) != 0 || ad.Bridge == 0 {
		t.Fatalf("while off: %+v %v", ad, err)
	}
	for _, d := range []*sold.Daemon{a, b} {
		if st, err := d.SetBLE(true); err != nil || st.BLE == nil || !st.BLE.Enabled {
			t.Fatalf("SetBLE: %+v %v", st, err)
		}
	}
	lo, hi := a, b
	if bytes.Compare(dev(t, a), dev(t, b)) > 0 {
		lo, hi = b, a
	}
	adLo, err := lo.BLEAdFor(0x81)
	if err != nil || len(adLo.Adverts) != 4 {
		t.Fatalf("ad: %+v %v", adLo, err)
	}
	adHi, _ := hi.BLEAdFor(0x82)
	plan, err := lo.BLEMatch([]nearby.BLESeen{{ID: "hi", Data: adHi.Adverts}})
	if err != nil || !plan.Scan || len(plan.Actions) != 1 || plan.Actions[0].Do != "connect" || plan.Actions[0].PSM != 0x82 || len(plan.Actions[0].Link) != 32 {
		t.Fatalf("lower key: %+v %v", plan, err)
	}
	if p, _ := hi.BLEMatch([]nearby.BLESeen{{ID: "lo", Data: adLo.Adverts}}); len(p.Actions) != 0 || len(p.Matched) != 1 {
		t.Fatalf("higher key: %+v", p)
	}
	s, _ := nearbyStart(t, "", sold.InitOptions{Name: "Sam", Device: "phone"}, nearby.NewBus())
	s.SetBLE(true)
	adS, _ := s.BLEAdFor(0x83)
	if p, _ := lo.BLEMatch([]nearby.BLESeen{{ID: "s", Data: adS.Adverts}}); len(p.Matched) != 0 || len(p.Actions) != 0 {
		t.Fatalf("stranger: %+v", p)
	}

	// The shells bridge the channel; the session comes up over it.
	bridge := func(port int, line string) net.Conn {
		c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		io.WriteString(c, line)
		return c
	}
	out := bridge(adLo.Bridge, "SOLBLE1 out "+plan.Actions[0].Link+"\n")
	in := bridge(adHi.Bridge, "SOLBLE1 in\n")
	go func() { io.Copy(out, in); out.Close() }()
	go func() { io.Copy(in, out); in.Close() }()
	eventually(t, "a session over BLE", func() bool {
		ses, ok := lo.Sessions.Session(dev(t, hi))
		return ok && ses.Alive() && ses.Path().Transport == "ble"
	})
	eventually(t, "the other side knows it came over BLE", func() bool {
		ses, ok := hi.Sessions.Session(dev(t, lo))
		return ok && ses.Path().Via == "ble"
	})
	// Connected over BLE only: no new link, still scanning for an IP path, still advertising.
	if p, _ := lo.BLEMatch([]nearby.BLESeen{{ID: "hi", Data: adHi.Adverts}}); len(p.Actions) != 0 || !p.Scan {
		t.Fatalf("over BLE: %+v", p)
	}
	if ad, _ := lo.BLEAdFor(0x81); len(ad.Adverts) == 0 {
		t.Fatal("stopped advertising with only a BLE session")
	}

	// Off: the link is cut.
	ses, _ := lo.Sessions.Session(dev(t, hi))
	lo.SetBLE(false)
	eventually(t, "the BLE session ended", func() bool { return !ses.Alive() })
}
