package faultnet

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/apv022/sol/internal/runtime"
)

// The fake tor. A test binary becomes it when it is started with
// fakeTorEnv set: call MaybeRunFakeTor first thing in TestMain. The node's
// Tor transport then runs the test binary itself as "tor" (FakeTor.Provider)
// and talks to it over a real control port and a real SOCKS port.
//
// What it does like the real thing:
//   - reads the torrc it is given (SocksPort, ControlPort, CookieAuthFile),
//     writes the cookie, and answers AUTHENTICATE, TAKEOWNERSHIP, GETINFO
//     status/bootstrap-phase, ADD_ONION, DEL_ONION, ONION_CLIENT_AUTH_ADD /
//     _REMOVE, HSFETCH and SETEVENTS;
//   - exits when the control connection that took ownership closes, or when
//     the process that owns it is gone;
//   - carries SOCKS connections to "<id>.onion" to the target of the fake
//     tor that published that onion (all fake tors sharing one directory
//     form a tiny "Tor network"), while that fake tor is alive.
//
// What a test scripts, per launch (TorPlan): where bootstrap stalls, when
// the process exits, when the control port drops or freezes, and whether
// the descriptor is ever reported as uploaded.

const fakeTorEnv = "SOL_FAULTNET_FAKETOR"

// TorPlan scripts one launch of the fake tor. The zero value is a healthy
// tor: it bootstraps to 100% in a few milliseconds and reports its onions
// as uploaded right away.
type TorPlan struct {
	// ExitAtStart exits with status 1 before the control port opens (a
	// broken install, or a data directory tor refuses).
	ExitAtStart bool `json:"exit_at_start,omitempty"`
	// NoControl never opens the control port; the process just sits there.
	NoControl bool `json:"no_control,omitempty"`
	// StallAt stops bootstrap at this percentage for good (0: no stall).
	StallAt int `json:"stall_at,omitempty"`
	// StepMS is how long each 10% of bootstrap takes (default 5 ms).
	StepMS int `json:"step_ms,omitempty"`
	// ExitAfterMS exits (status 1) this long after starting (0: never).
	ExitAfterMS int `json:"exit_after_ms,omitempty"`
	// DropControlAfterMS closes every control connection, and stops taking
	// new ones, this long after starting, while the process lives on.
	DropControlAfterMS int `json:"drop_control_after_ms,omitempty"`
	// HangControlAfterMS stops answering control commands this long after
	// starting (a frozen process), without closing anything.
	HangControlAfterMS int `json:"hang_control_after_ms,omitempty"`
	// NoUpload never reports an onion's descriptor as uploaded, and makes
	// connections to this tor's onions fail: the address never goes live.
	NoUpload bool `json:"no_upload,omitempty"`
	// Warning is reported with the bootstrap phase, as tor does when it has
	// trouble ("Clock skew ..."), with Reason as its REASON= code.
	Warning string `json:"warning,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// StuckFetch makes this tor's descriptor lookups hang, as when the
	// circuit to the one directory tor asked stalls: a SOCKS connection to
	// an onion waits until the controller sends HSFETCH for it (which then
	// "answers" with HS_DESC RECEIVED), or until the client gives up.
	StuckFetch bool `json:"stuck_fetch,omitempty"`
}

// FakeTor controls the fake tors started from one directory.
type FakeTor struct {
	dir string
}

// NewFakeTor returns a controller keeping its script and state in dir.
func NewFakeTor(dir string) *FakeTor {
	os.MkdirAll(filepath.Join(dir, "onions"), 0o700)
	return &FakeTor{dir: dir}
}

// Provider is what to give transport.NewTor: it runs this test binary as
// the fake tor.
func (f *FakeTor) Provider() runtime.Provider {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	return &runtime.Exec{Path: exe, Env: []string{fakeTorEnv + "=" + f.dir}}
}

// Script sets the plan of each launch from now on: the first launch after
// this call follows plans[0], the next plans[1], and so on; the last plan
// repeats. No plans means every launch is healthy.
func (f *FakeTor) Script(plans ...TorPlan) {
	b, _ := json.Marshal(struct {
		From  int       `json:"from"`
		Plans []TorPlan `json:"plans"`
	}{f.Launches(), plans})
	tmp := filepath.Join(f.dir, "script.tmp")
	os.WriteFile(tmp, b, 0o600)
	os.Rename(tmp, filepath.Join(f.dir, "script.json"))
}

// Launches reports how many times the fake tor has been started.
func (f *FakeTor) Launches() int {
	b, _ := os.ReadFile(filepath.Join(f.dir, "launches"))
	return strings.Count(string(b), "\n")
}

// Kill kills the newest fake tor process, as the system would (an Android
// phone reclaiming memory), and reports whether one was running.
func (f *FakeTor) Kill() bool {
	b, _ := os.ReadFile(filepath.Join(f.dir, "launches"))
	lines := strings.Fields(string(b))
	if len(lines) == 0 {
		return false
	}
	pid, err := strconv.Atoi(lines[len(lines)-1])
	if err != nil {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Kill() == nil
}

// MaybeRunFakeTor turns this process into the fake tor when it was started
// as one, and never returns in that case. Call it at the top of TestMain.
func MaybeRunFakeTor() {
	dir := os.Getenv(fakeTorEnv)
	if dir == "" {
		return
	}
	os.Exit(runFakeTor(dir, os.Args[1:]))
}

// alive reports whether process pid exists.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	defer p.Release()
	if goruntime.GOOS == "windows" {
		return true // FindProcess opens the process there: it exists
	}
	return p.Signal(syscall.Signal(0)) == nil
}

type fakeTor struct {
	dir  string
	plan TorPlan
	t0   time.Time

	mu      sync.Mutex
	onions  map[string]string // service id -> target
	conns   map[net.Conn]struct{}
	events  []*textproto.Conn        // connections that asked for HS_DESC events
	fetched map[string]chan struct{} // StuckFetch: closed once HSFETCH asked for the onion
	dropped bool
	exit    chan int
}

func runFakeTor(dir string, args []string) int {
	torrc := ""
	for i, a := range args {
		if a == "-f" && i+1 < len(args) {
			torrc = args[i+1]
		}
	}
	conf := map[string]string{}
	if f, err := os.Open(torrc); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			// The first value of the first line for a key, like tor's
			// first port; flags such as ExtendedErrors are ignored.
			if f := strings.Fields(sc.Text()); len(f) >= 2 && conf[f[0]] == "" {
				conf[f[0]] = f[1]
			}
		}
		f.Close()
	}
	if conf["ControlPort"] == "" || conf["SocksPort"] == "" || conf["CookieAuthFile"] == "" {
		fmt.Println("fake tor: torrc lacks ControlPort, SocksPort or CookieAuthFile")
		return 2
	}

	// Which launch is this? One line per launch, appended atomically.
	lf, err := os.OpenFile(filepath.Join(dir, "launches"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Println("fake tor:", err)
		return 2
	}
	before, _ := os.ReadFile(filepath.Join(dir, "launches"))
	n := strings.Count(string(before), "\n")
	fmt.Fprintf(lf, "%d\n", os.Getpid())
	lf.Close()

	var script struct {
		From  int       `json:"from"`
		Plans []TorPlan `json:"plans"`
	}
	if b, err := os.ReadFile(filepath.Join(dir, "script.json")); err == nil {
		json.Unmarshal(b, &script)
	}
	ft := &fakeTor{dir: dir, t0: time.Now(), onions: map[string]string{}, conns: map[net.Conn]struct{}{}, exit: make(chan int, 4)}
	if len(script.Plans) > 0 {
		i := min(max(n-script.From, 0), len(script.Plans)-1)
		ft.plan = script.Plans[i]
	}
	fmt.Printf("fake tor: launch %d, plan %+v\n", n+1, ft.plan)
	if ft.plan.ExitAtStart {
		fmt.Println("fake tor: exiting at start, as scripted")
		return 1
	}
	defer ft.forgetOnions()

	// Like tor's __OwningControllerProcess: never outlive the node.
	if pid, err := strconv.Atoi(conf["__OwningControllerProcess"]); err == nil {
		go func() {
			for {
				time.Sleep(200 * time.Millisecond)
				if !alive(pid) {
					ft.exit <- 0
					return
				}
			}
		}()
	}
	if ms := ft.plan.ExitAfterMS; ms > 0 {
		time.AfterFunc(time.Duration(ms)*time.Millisecond, func() { ft.exit <- 1 })
	}

	socks, err := net.Listen("tcp", conf["SocksPort"])
	if err != nil {
		fmt.Println("fake tor:", err)
		return 2
	}
	defer socks.Close()
	go ft.serveSOCKS(socks)

	if !ft.plan.NoControl {
		cookie := make([]byte, 32)
		rand.Read(cookie)
		if err := os.WriteFile(conf["CookieAuthFile"], cookie, 0o600); err != nil {
			fmt.Println("fake tor:", err)
			return 2
		}
		ctl, err := net.Listen("tcp", conf["ControlPort"])
		if err != nil {
			fmt.Println("fake tor:", err)
			return 2
		}
		defer ctl.Close()
		go func() {
			for {
				c, err := ctl.Accept()
				if err != nil {
					return
				}
				go ft.serveControl(c)
			}
		}()
		if ms := ft.plan.DropControlAfterMS; ms > 0 {
			time.AfterFunc(time.Duration(ms)*time.Millisecond, func() {
				ctl.Close()
				ft.mu.Lock()
				ft.dropped = true
				for c := range ft.conns {
					c.Close()
				}
				ft.mu.Unlock()
			})
		}
	}
	code := <-ft.exit
	fmt.Println("fake tor: exiting with status", code)
	return code
}

// progress is the bootstrap percentage right now.
func (ft *fakeTor) progress() int {
	step := time.Duration(ft.plan.StepMS) * time.Millisecond
	if step <= 0 {
		step = 5 * time.Millisecond
	}
	p := min(int(time.Since(ft.t0)/step)*10, 100)
	if ft.plan.StallAt > 0 {
		p = min(p, ft.plan.StallAt)
	}
	return p
}

func (ft *fakeTor) hung() bool {
	ms := ft.plan.HangControlAfterMS
	return ms > 0 && time.Since(ft.t0) > time.Duration(ms)*time.Millisecond
}

func (ft *fakeTor) onionFile(sid string) string { return filepath.Join(ft.dir, "onions", sid) }

func (ft *fakeTor) forgetOnions() {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	for sid := range ft.onions {
		os.Remove(ft.onionFile(sid))
	}
}

func (ft *fakeTor) serveControl(c net.Conn) {
	ft.mu.Lock()
	if ft.dropped {
		ft.mu.Unlock()
		c.Close()
		return
	}
	ft.conns[c] = struct{}{}
	ft.mu.Unlock()
	owner := false
	defer func() {
		c.Close()
		ft.mu.Lock()
		delete(ft.conns, c)
		dropped := ft.dropped
		ft.mu.Unlock()
		// Real tor exits when the owning connection closes. (A scripted drop
		// is the exception: it stands for a control port that died while the
		// process lived on.)
		if owner && !dropped {
			ft.exit <- 0
		}
	}()
	tp := textproto.NewConn(c)
	var wmu sync.Mutex
	reply := func(format string, args ...any) {
		wmu.Lock()
		defer wmu.Unlock()
		tp.PrintfLine(format, args...)
	}
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		if ft.hung() {
			select {} // frozen: never answer, never close
		}
		cmd, rest, _ := strings.Cut(line, " ")
		switch strings.ToUpper(cmd) {
		case "AUTHENTICATE", "ONION_CLIENT_AUTH_ADD", "ONION_CLIENT_AUTH_REMOVE":
			reply("250 OK")
		case "TAKEOWNERSHIP":
			owner = true
			reply("250 OK")
		case "GETINFO":
			if rest != "status/bootstrap-phase" {
				reply("552 Unrecognized key %q", rest)
				continue
			}
			p := ft.progress()
			sev, extra := "NOTICE", ""
			if ft.plan.Warning != "" && p < 100 {
				reason := ft.plan.Reason
				if reason == "" {
					reason = "NOROUTE"
				}
				sev = "WARN"
				extra = fmt.Sprintf(` WARNING="%s" REASON=%s COUNT=3 RECOMMENDATION=warn`, ft.plan.Warning, reason)
			}
			tag, summary := "conn", "Connecting to a relay"
			switch {
			case p >= 100:
				tag, summary = "done", "Done"
			case p >= 50:
				tag, summary = "loading_descriptors", "Loading relay descriptors"
			case p >= 20:
				tag, summary = "onehop_create", "Establishing an encrypted directory connection"
			}
			reply(`250-status/bootstrap-phase=%s BOOTSTRAP PROGRESS=%d TAG=%s SUMMARY="%s"%s`, sev, p, tag, summary, extra)
			reply("250 OK")
		case "SETEVENTS":
			reply("250 OK")
			ft.mu.Lock()
			ft.events = append(ft.events, tp)
			var have []string
			for sid := range ft.onions {
				have = append(have, sid)
			}
			ft.mu.Unlock()
			for _, sid := range have {
				go ft.upload(sid)
			}
		case "ADD_ONION":
			f := strings.Fields(rest)
			if len(f) < 2 {
				reply("512 Bad arguments to ADD_ONION")
				continue
			}
			key, target, isNew := f[0], "", strings.HasPrefix(f[0], "NEW:")
			if isNew {
				raw := make([]byte, 32)
				rand.Read(raw)
				key = "ED25519-V3:" + base32.StdEncoding.EncodeToString(raw)
			}
			for _, a := range f[1:] {
				if v, ok := strings.CutPrefix(a, "Port="); ok {
					_, target, _ = strings.Cut(v, ",")
				}
			}
			if strings.Contains(rest, "ClientAuthV3=") && !strings.Contains(rest, "Flags=V3Auth") {
				reply("512 No auth type specified")
				continue
			}
			sum := sha256.Sum256([]byte(key))
			sid := strings.ToLower(base32.StdEncoding.EncodeToString(sum[:]))[:52] + "fake"
			ft.mu.Lock()
			ft.onions[sid] = target
			ft.mu.Unlock()
			if !ft.plan.NoUpload {
				os.WriteFile(ft.onionFile(sid), []byte(fmt.Sprintf("%d %s", os.Getpid(), target)), 0o600)
			}
			reply("250-ServiceID=%s", sid)
			if isNew {
				reply("250-PrivateKey=%s", key)
			}
			reply("250 OK")
			go ft.upload(sid)
		case "HSFETCH":
			sid := strings.TrimSuffix(strings.Fields(rest + " x")[0], ".onion")
			reply("250 OK")
			ft.mu.Lock()
			if ch := ft.fetchedLocked(sid); !isClosed(ch) {
				close(ch)
			}
			for _, tp := range ft.events {
				tp.PrintfLine("650 HS_DESC RECEIVED %s NO_AUTH $FAKEDIR descid", sid)
			}
			ft.mu.Unlock()
		case "DEL_ONION":
			ft.mu.Lock()
			delete(ft.onions, rest)
			ft.mu.Unlock()
			os.Remove(ft.onionFile(rest))
			reply("250 OK")
		default:
			reply("510 Unrecognized command %q", cmd)
		}
	}
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// fetchedLocked is the channel HSFETCH for sid closes (StuckFetch); ft.mu
// is held.
func (ft *fakeTor) fetchedLocked(sid string) chan struct{} {
	if ft.fetched == nil {
		ft.fetched = map[string]chan struct{}{}
	}
	ch, ok := ft.fetched[sid]
	if !ok {
		ch = make(chan struct{})
		ft.fetched[sid] = ch
	}
	return ch
}

// upload reports sid's descriptor as uploaded once bootstrap is done, to
// every connection listening for events.
func (ft *fakeTor) upload(sid string) {
	if ft.plan.NoUpload {
		return
	}
	for ft.progress() < 100 {
		if ft.plan.StallAt > 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	ft.mu.Lock()
	defer ft.mu.Unlock()
	if _, ok := ft.onions[sid]; !ok || ft.dropped {
		return
	}
	for _, tp := range ft.events {
		tp.PrintfLine("650 HS_DESC UPLOAD %s UNKNOWN $FAKEDIR descid", sid)
		tp.PrintfLine("650 HS_DESC UPLOADED %s UNKNOWN $FAKEDIR", sid)
	}
}

func (ft *fakeTor) serveSOCKS(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			target, err := socksRequest(c)
			if err != nil {
				c.Close()
				return
			}
			fail := func(code byte) {
				c.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0})
				c.Close()
			}
			host, _, _ := net.SplitHostPort(target)
			sid, isOnion := strings.CutSuffix(host, ".onion")
			if !isOnion || ft.progress() < 100 {
				fail(1)
				return
			}
			if ft.plan.StuckFetch {
				ft.mu.Lock()
				fetched := ft.fetchedLocked(sid)
				ft.mu.Unlock()
				// Hang like a stalled directory fetch until HSFETCH; give up
				// like tor's SocksTimeout otherwise.
				select {
				case <-fetched:
				case <-time.After(time.Minute):
					fail(6) // TTL expired
					return
				}
			}
			// The onion is up while the fake tor that published it is alive.
			b, err := os.ReadFile(ft.onionFile(sid))
			pidStr, addr, _ := strings.Cut(string(b), " ")
			pid, _ := strconv.Atoi(pidStr)
			if err != nil || !alive(pid) {
				fail(0xF0) // descriptor not found
				return
			}
			out, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				fail(5)
				return
			}
			if _, err := c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
				c.Close()
				out.Close()
				return
			}
			go func() { io.Copy(out, c); out.Close() }()
			io.Copy(c, out)
			c.Close()
		}()
	}
}
