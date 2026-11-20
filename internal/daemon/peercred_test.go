package daemon

import (
	"log/slog"
	"net/http"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const procTCP4 = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1E61 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 11111 1 0000000000000000 100 0 0 10 0
   1: 0100007F:C350 0100007F:1E61 01 00000000:00000000 00:00000000 00000000  1001        0 22222 1 0000000000000000 20 4 30 10 -1
   2: 0100007F:1E61 0100007F:C350 01 00000000:00000000 00:00000000 00000000  1000        0 33333 1 0000000000000000 20 4 30 10 -1
`

const procTCP6 = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:1E61 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 1 1 0000000000000000 100 0 0 10 0
   1: 00000000000000000000000001000000:D431 00000000000000000000000001000000:1E61 01 00000000:00000000 00:00000000 00000000  1002        0 2 1 0000000000000000 20 4 30 10 -1
   2: 0000000000000000FFFF00000100007F:D432 0000000000000000FFFF00000100007F:1E61 01 00000000:00000000 00:00000000 00000000  1003        0 3 1 0000000000000000 20 4 30 10 -1
`

func TestParseProcNetUID(t *testing.T) {
	ap := netip.MustParseAddrPort
	// The client end (local 127.0.0.1:50000 -> server :7777) is uid 1001, not the server's 1000.
	if uid, ok := parseProcNetUID(strings.NewReader(procTCP4), ap("127.0.0.1:50000"), ap("127.0.0.1:7777")); !ok || uid != 1001 {
		t.Fatalf("v4: %d %v", uid, ok)
	}
	if _, ok := parseProcNetUID(strings.NewReader(procTCP4), ap("127.0.0.1:50001"), ap("127.0.0.1:7777")); ok {
		t.Fatal("matched an unknown port")
	}
	if uid, ok := parseProcNetUID(strings.NewReader(procTCP6), ap("[::1]:54321"), ap("[::1]:7777")); !ok || uid != 1002 {
		t.Fatalf("v6: %d %v", uid, ok)
	}
	// An IPv4 client on a dual-stack socket appears as ::ffff:127.0.0.1.
	if uid, ok := parseProcNetUID(strings.NewReader(procTCP6), ap("127.0.0.1:54322"), ap("127.0.0.1:7777")); !ok || uid != 1003 {
		t.Fatalf("mapped: %d %v", uid, ok)
	}
	// Security review F5: a TIME_WAIT row of an earlier connection on the same ports (uid 0)
	// is not the owner; the live row after it is.
	const tw = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:C350 0100007F:1E61 06 00000000:00000000 03:00000000 00000000     0        0 0 3 0000000000000000
   1: 0100007F:C350 0100007F:1E61 01 00000000:00000000 00:00000000 00000000  1001        0 22222 1 0000000000000000 20 4 30 10 -1
`
	if uid, ok := parseProcNetUID(strings.NewReader(tw), ap("127.0.0.1:50000"), ap("127.0.0.1:7777")); !ok || uid != 1001 {
		t.Fatalf("time-wait: %d %v", uid, ok)
	}
	if _, ok := parseProcNetUID(strings.NewReader(tw[:strings.Index(tw, "   1:")]), ap("127.0.0.1:50000"), ap("127.0.0.1:7777")); ok {
		t.Fatal("a TIME_WAIT row named an owner")
	}
	if _, ok := parseProcNetUID(strings.NewReader("garbage\nnot a row\n"), ap("127.0.0.1:1"), ap("127.0.0.1:2")); ok {
		t.Fatal("garbage matched")
	}
}

// TestAutoLogin: the same user is let in without a code, another user and a failed lookup are
// not, and a cross-site navigation or an app origin never triggers it.
func TestAutoLogin(t *testing.T) {
	start := func(f SameUserFunc) *Daemon {
		dir := t.TempDir()
		cfg := Config{UI: "127.0.0.1:0", Files: filepath.Join(dir, "files"), Transports: []TransportConfig{{Name: "clearnet", Listen: "127.0.0.1:0"}}}
		if err := Init(dir, InitOptions{Name: "A", Device: "d", Config: cfg}); err != nil {
			t.Fatal(err)
		}
		d, err := Start(Options{Dir: dir, InProcessApps: true, APIOverTCP: true, Log: slog.New(slog.DiscardHandler), SameUser: f})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.Close() })
		return d
	}
	var seen netip.AddrPort
	same := start(func(remote, local netip.AddrPort) (bool, error) { seen = remote; return true, nil })
	other := start(func(_, _ netip.AddrPort) (bool, error) { return false, nil })
	broken := start(func(_, _ netip.AddrPort) (bool, error) { return false, ErrSameUserUnsupported })

	root := func(d *Daemon) string { return "localhost:" + strconv.Itoa(d.UIPort()) }
	cookieOf := func(r *http.Response) string {
		for _, c := range r.Cookies() {
			if c.Name == SessionCookie && c.Value != "" {
				return c.Value
			}
		}
		return ""
	}

	// Page load: cookie set, and it works on the admin API with a proper Origin.
	r := uiReq(t, same, "GET", root(same), "/", "", "", "", nil)
	ck := cookieOf(r)
	if r.StatusCode != 200 || ck == "" || !seen.Addr().IsLoopback() {
		t.Fatalf("same user page: %d cookie=%q seen=%v", r.StatusCode, ck, seen)
	}
	if r := uiReq(t, same, "GET", root(same), "/admin/v1/status", "http://"+root(same), ck, "", nil); r.StatusCode != 200 {
		t.Fatalf("session from auto-login: %d", r.StatusCode)
	}
	// Admin API without a cookie: also signs in.
	r = uiReq(t, same, "GET", root(same), "/admin/v1/status", "http://"+root(same), "", "", nil)
	if r.StatusCode != 200 || cookieOf(r) == "" {
		t.Fatalf("admin auto-login: %d", r.StatusCode)
	}
	// A cross-site navigation or a wrong Origin does not.
	if r := uiReq(t, same, "GET", root(same), "/", "", "", "", map[string]string{"Sec-Fetch-Site": "cross-site"}); cookieOf(r) != "" {
		t.Fatal("cross-site navigation signed in")
	}
	if r := uiReq(t, same, "GET", root(same), "/admin/v1/status", "http://evil.example", "", "", nil); r.StatusCode != 403 || cookieOf(r) != "" {
		t.Fatalf("foreign origin: %d", r.StatusCode)
	}
	// An app origin never gets the admin cookie.
	app := "files.localhost:" + strconv.Itoa(same.UIPort())
	if r := uiReq(t, same, "GET", app, "/", "", "", "", nil); cookieOf(r) != "" {
		t.Fatal("app origin got a session")
	}
	// Another user, or no way to tell: the login screen.
	for name, d := range map[string]*Daemon{"other": other, "unsupported": broken} {
		r := uiReq(t, d, "GET", root(d), "/", "", "", "", nil)
		if cookieOf(r) != "" {
			t.Fatalf("%s: signed in", name)
		}
		if r := uiReq(t, d, "GET", root(d), "/admin/v1/status", "http://"+root(d), "", "", nil); r.StatusCode != 401 {
			t.Fatalf("%s: admin %d", name, r.StatusCode)
		}
	}
}
