package transport

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apv022/sol/internal/faultnet"
)

// The supervisor tests run the real Tor transport against faultnet's fake
// tor (this test binary, re-executed as "tor"), with every timing shortened.

func TestMain(m *testing.M) {
	faultnet.MaybeRunFakeTor()
	os.Exit(m.Run())
}

// setVar sets *p to v for the rest of the test.
func setVar[T any](t *testing.T, p *T, v T) {
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// supervised is a Tor transport on a fake tor, with fast timings, a network
// the test controls, and the Notify messages it sent.
type supervised struct {
	*Tor
	fake    *faultnet.FakeTor
	online  *atomic.Bool
	mu      sync.Mutex
	notices []string
}

func newSupervised(t *testing.T, plans ...faultnet.TorPlan) *supervised {
	t.Helper()
	online := &atomic.Bool{}
	online.Store(true)
	setVar(t, &torOnline, online.Load)
	setVar(t, &torPollEvery, 10*time.Millisecond)
	setVar(t, &torControlWait, 10*time.Second)
	setVar(t, &torControlTimeout, 300*time.Millisecond)
	setVar(t, &bootstrapStall, 250*time.Millisecond)
	setVar(t, &torRetryBase, 20*time.Millisecond)
	setVar(t, &torRetryMax, 200*time.Millisecond)
	setVar(t, &netSettle, 20*time.Millisecond)
	setVar(t, &netSettleMax, 200*time.Millisecond)
	setVar(t, &netRestartGap, 0)
	fake := faultnet.NewFakeTor(t.TempDir())
	fake.Script(plans...)
	s := &supervised{Tor: NewTor(t.TempDir(), fake.Provider()), fake: fake, online: online}
	s.Notify = func(msg string) {
		s.mu.Lock()
		s.notices = append(s.notices, msg)
		s.mu.Unlock()
	}
	return s
}

// start starts the transport, and stops it when the test ends. Timings
// must be set before: Cleanup runs Stop first, then restores them, so no
// goroutine of this tor is left to read them.
func (s *supervised) start(t *testing.T) error {
	t.Cleanup(func() { s.Stop() })
	return s.Start(context.Background())
}

// noticed reports whether some Notify message contained sub.
func (s *supervised) noticed(sub string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.notices {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

// waitStatus polls Status until ok accepts it.
func waitStatus(t *testing.T, tor *Tor, what string, ok func(Status) bool) Status {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		st := tor.Status()
		if ok(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; status %+v", what, st)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func isReady(st Status) bool { return st.Phase == PhaseReady && st.State == StateReady }

// A bootstrap stuck at one percentage is given up on and tor is started
// again; after purgeAfterStalls stalls the cached directory goes, the
// guard state stays.
func TestTorStalledBootstrapRestarts(t *testing.T) {
	s := newSupervised(t, faultnet.TorPlan{StallAt: 40}, faultnet.TorPlan{StallAt: 40}, faultnet.TorPlan{})
	setVar(t, &purgeAfterStalls, 2)
	data := filepath.Join(s.DataDir, "data")
	os.MkdirAll(data, 0o700)
	os.WriteFile(filepath.Join(data, "cached-consensus"), []byte("old"), 0o600)
	os.WriteFile(filepath.Join(data, "state"), []byte("guards"), 0o600)

	if err := s.start(t); err != nil {
		t.Fatal(err)
	}
	st := waitStatus(t, s.Tor, "bootstrap at 40%", func(st Status) bool { return st.Progress == 40 })
	if st.Phase != PhaseBootstrapping || st.State != StateStarting || st.Attempt != 1 ||
		st.PhaseSince.IsZero() || !strings.Contains(st.WaitingFor, "connect") || !st.RetryAt.IsZero() {
		t.Fatalf("stalled bootstrap reported as %+v", st)
	}
	st = waitStatus(t, s.Tor, "ready after the stalls", isReady)
	if n := s.fake.Launches(); n != 3 {
		t.Fatalf("launches = %d, want 3", n)
	}
	if st.Progress != 100 || st.Attempt != 1 || st.Restarts != 2 || st.Problem != "" || st.WaitingFor != "" {
		t.Fatalf("recovered tor reported as %+v", st)
	}
	if !s.noticed("no progress") {
		t.Fatalf("the stall was not reported: %q", s.notices)
	}
	if _, err := os.Stat(filepath.Join(data, "cached-consensus")); !os.IsNotExist(err) {
		t.Error("cached directory kept after repeated stalls")
	}
	if _, err := os.Stat(filepath.Join(data, "state")); err != nil {
		t.Error("guard state deleted:", err)
	}
}

// Without a network nothing can move, so a stuck bootstrap is not a stall
// and tor is left alone; the stall clock starts once there is a network.
func TestTorOfflineIsNotAStall(t *testing.T) {
	s := newSupervised(t, faultnet.TorPlan{StallAt: 40}, faultnet.TorPlan{})
	s.online.Store(false)
	if err := s.start(t); err != nil {
		t.Fatal(err)
	}
	// Offline is reported from the first poll on; wait until bootstrap
	// has also reached the scripted stall (a slow machine polls earlier).
	st := waitStatus(t, s.Tor, "offline at the stall", func(st Status) bool { return st.Phase == PhaseOffline && st.Progress == 40 })
	if st.WaitingFor != "a network connection" {
		t.Fatalf("offline reported as %+v", st)
	}
	time.Sleep(2 * bootstrapStall)
	if n := s.fake.Launches(); n != 1 {
		t.Fatalf("tor restarted %d times while offline", n-1)
	}
	s.online.Store(true)
	waitStatus(t, s.Tor, "ready once online", isReady)
	if n := s.fake.Launches(); n != 2 {
		t.Fatalf("launches = %d, want 2", n)
	}
}

// A tor that keeps dying is restarted with a doubling backoff, capped;
// one that dies after working is restarted at once without a penalty.
func TestTorExitRestartsWithBackoff(t *testing.T) {
	s := newSupervised(t, faultnet.TorPlan{StallAt: 30, ExitAfterMS: 150})
	setVar(t, &torRetryBase, 60*time.Millisecond)
	setVar(t, &torRetryMax, 240*time.Millisecond)
	s.start(t) // may fail if the fake dies first: retried either way

	waits := map[int]time.Duration{}
	for len(waits) < 4 {
		st := waitStatus(t, s.Tor, "retrying", func(st Status) bool { return st.Phase == PhaseRetrying && waits[st.Attempt] == 0 })
		if st.RetryAt.IsZero() || st.WaitingFor == "" || st.Problem == "" || st.State == StateReady {
			t.Fatalf("retrying reported as %+v", st)
		}
		waits[st.Attempt] = st.RetryAt.Sub(st.PhaseSince).Round(10 * time.Millisecond)
	}
	want := map[int]time.Duration{2: 60 * time.Millisecond, 3: 120 * time.Millisecond, 4: 240 * time.Millisecond, 5: 240 * time.Millisecond}
	for a, w := range want {
		if waits[a] != w {
			t.Errorf("attempt %d waited %v, want %v (all: %v)", a, waits[a], w, waits)
		}
	}
	if !s.noticed("exited") {
		t.Errorf("exits not reported: %q", s.notices)
	}

	s.fake.Script(faultnet.TorPlan{})
	st := waitStatus(t, s.Tor, "ready", isReady)
	if st.Attempt != 1 || st.Problem != "" {
		t.Fatalf("recovered tor reported as %+v", st)
	}
	before, restarts := s.fake.Launches(), st.Restarts
	if !s.fake.Kill() {
		t.Fatal("no fake tor to kill")
	}
	st = waitStatus(t, s.Tor, "ready after the kill", func(st Status) bool { return isReady(st) && st.Restarts > restarts })
	if n := s.fake.Launches(); n != before+1 || st.Attempt != 1 {
		t.Fatalf("after a kill: launches %d (was %d), status %+v", n, before, st)
	}
}

// A control port that drops, or stops answering, while the process lives
// on is noticed and tor is replaced.
func TestTorControlLossRestarts(t *testing.T) {
	for name, plan := range map[string]faultnet.TorPlan{
		"dropped": {DropControlAfterMS: 300},
		"frozen":  {HangControlAfterMS: 300},
	} {
		t.Run(name, func(t *testing.T) {
			s := newSupervised(t, plan, faultnet.TorPlan{})
			if err := s.start(t); err != nil {
				t.Fatal(err)
			}
			waitStatus(t, s.Tor, "first ready", isReady)
			st := waitStatus(t, s.Tor, "ready again", func(st Status) bool { return isReady(st) && st.Restarts == 1 })
			if n := s.fake.Launches(); n != 2 || st.Attempt != 1 {
				t.Fatalf("launches %d, status %+v", n, st)
			}
			if !s.noticed("control port") {
				t.Fatalf("not reported: %q", s.notices)
			}
		})
	}
}

// NetworkChanged cuts a long backoff short. Publications made while tor is
// down are registered once it is up. A burst of changes restarts a working
// tor once, and netRestartGap holds off the next.
func TestTorNetworkChangedKicksBackoff(t *testing.T) {
	s := newSupervised(t, faultnet.TorPlan{ExitAtStart: true})
	setVar(t, &torRetryBase, time.Hour)
	setVar(t, &torRetryMax, time.Hour)
	setVar(t, &netRestartGap, time.Hour) // the first restart is not held off
	if err := s.start(t); err == nil {
		t.Fatal("Start reported success for a tor that exits at once")
	}
	st := waitStatus(t, s.Tor, "retrying", func(st Status) bool { return st.Phase == PhaseRetrying })
	if st.Attempt != 2 || st.RetryAt.Sub(st.PhaseSince) < 59*time.Minute || st.Restarts != 1 {
		t.Fatalf("retrying reported as %+v", st)
	}
	if _, err := s.Publish(context.Background(), Publication{ID: "node", Target: echoTarget(t)}); err == nil {
		t.Fatal("Publish without a tor and without a known address reported success")
	}

	s.fake.Script(faultnet.TorPlan{})
	s.NetworkChanged("Wi-Fi connected")
	st = waitStatus(t, s.Tor, "ready after the kick", isReady)
	if n := s.fake.Launches(); n != 2 || st.Addresses["node"] == "" {
		t.Fatalf("launches %d, status %+v", n, st)
	}

	for range 20 {
		s.NetworkChanged("flapping")
		time.Sleep(time.Millisecond)
	}
	waitStatus(t, s.Tor, "ready after one restart", func(st Status) bool { return isReady(st) && s.fake.Launches() == 3 })
	for range 5 {
		s.NetworkChanged("flapping again")
	}
	time.Sleep(10 * netSettle)
	if n := s.fake.Launches(); n != 3 {
		t.Fatalf("a flapping network restarted tor %d times, want 1", n-2)
	}
	start := time.Now()
	s.Stop() // netLoop is waiting out netRestartGap
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Stop took %v", d)
	}
}

// Stop during a long backoff returns at once and ends every goroutine.
func TestTorStopWhileRetrying(t *testing.T) {
	s := newSupervised(t, faultnet.TorPlan{ExitAtStart: true})
	setVar(t, &torRetryBase, time.Hour)
	s.start(t)
	waitStatus(t, s.Tor, "retrying", func(st Status) bool { return st.Phase == PhaseRetrying })
	start := time.Now()
	s.Stop() // waits for the supervisor's goroutines (Tor.wg)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Stop took %v", d)
	}
	if st := s.Status(); st.Phase != PhaseStopped || st.State != StateStopped || !st.RetryAt.IsZero() || st.WaitingFor != "" {
		t.Fatalf("stopped tor reported as %+v", st)
	}
}

// Bootstrapped but not yet announced is "publishing", not "ready"; the
// publication survives a restart with the same address.
func TestTorPublishingPhase(t *testing.T) {
	s := newSupervised(t, faultnet.TorPlan{NoUpload: true}, faultnet.TorPlan{})
	if err := s.start(t); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, s.Tor, "ready with nothing published", isReady)
	addr, err := s.Publish(context.Background(), Publication{ID: "node", Target: echoTarget(t)})
	if err != nil {
		t.Fatal(err)
	}
	st := waitStatus(t, s.Tor, "publishing", func(st Status) bool { return st.Phase == PhasePublishing })
	if st.State != StateStarting || !strings.Contains(st.WaitingFor, "address") || st.Progress != 100 {
		t.Fatalf("publishing reported as %+v", st)
	}
	s.Restart("test")
	st = waitStatus(t, s.Tor, "ready after the restart", func(st Status) bool { return isReady(st) && s.fake.Launches() == 2 })
	if st.Addresses["node"] != addr {
		t.Fatalf("address changed across a restart: %q -> %q", addr, st.Addresses["node"])
	}
}

// CLOCK_SKEW is shown to the user; CLOCK_JUMPED counts as a network change.
func TestTorStatusGeneralEvents(t *testing.T) {
	tor := NewTor(t.TempDir(), nil)
	tor.noteEvent("650 STATUS_GENERAL WARN CLOCK_SKEW SKEW=-7200 SOURCE=DIRSERV:1.2.3.4:80")
	if !strings.Contains(tor.clockSkew, "2h0m0s behind") {
		t.Fatalf("clock skew message %q", tor.clockSkew)
	}
	tor.noteEvent("650 STATUS_GENERAL NOTICE CLOCK_JUMPED TIME=3600")
	select {
	case ev := <-tor.netc:
		if ev.reason == "" {
			t.Fatal("no reason")
		}
	default:
		t.Fatal("CLOCK_JUMPED did not count as a network change")
	}
	tor.noteEvent("650 STATUS_GENERAL") // truncated: ignored
}

// Regression: WaitReady used to return an error as soon as tor was marked
// failed (three starts in a row), though the supervisor keeps retrying and
// tor can still come up. It must keep waiting, say it is retrying if ctx
// ends first, and still return once tor works, or at once on Stop.
func TestTorWaitReadyOutlastsFailed(t *testing.T) {
	s := newSupervised(t, faultnet.TorPlan{ExitAtStart: true})
	s.start(t) // fails: the fake exits at once; the supervisor retries
	waitStatus(t, s.Tor, "failed", func(st Status) bool { return st.State == StateFailed })

	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	err := s.WaitReady(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "still retrying") {
		t.Fatalf("WaitReady on a failed tor: %v, want the deadline with a note that tor is retrying", err)
	}

	done := make(chan error, 1)
	go func() { done <- s.WaitReady(context.Background()) }()
	s.fake.Script(faultnet.TorPlan{})
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitReady after tor recovered: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("WaitReady did not return after tor came up")
	}

	s.Stop()
	if err := s.WaitReady(context.Background()); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("WaitReady after Stop: %v", err)
	}
}
