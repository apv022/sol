//go:build linux

package netwatch

import (
	"context"
	"encoding/binary"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// syncBuf is a log sink safe for the watcher's goroutine.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestNetlinkForbidden: where the OS refuses the netlink socket (Android's SELinux: EACCES
// on bind), Watch says so once and neither retries nor scans the interfaces (that would be
// refused too, every PollEvery); a wake from sleep is still reported.
func TestNetlinkForbidden(t *testing.T) {
	var opens, scans atomic.Int32
	oldOpen, oldIfs := openNetlink, interfaces
	t.Cleanup(func() { openNetlink, interfaces = oldOpen, oldIfs })
	openNetlink = func() (int, error) {
		opens.Add(1)
		return -1, os.NewSyscallError("netlink bind", syscall.EACCES)
	}
	interfaces = func() ([]net.Interface, error) { scans.Add(1); return nil, nil }

	var jump atomic.Int64
	wall := func() time.Time { return time.Now().Round(0).Add(time.Duration(jump.Load())) }
	log := &syncBuf{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan string, 4)
	go Watch(ctx, Options{Debounce: 20 * time.Millisecond, SleepCheckEvery: 10 * time.Millisecond, SleepJump: time.Second,
		PollEvery: 5 * time.Millisecond, Wall: wall, Log: slog.New(slog.NewTextHandler(log, nil))}, func(r string) { got <- r })
	time.Sleep(150 * time.Millisecond)
	if n := opens.Load(); n != 1 {
		t.Fatalf("netlink opened %d times", n)
	}
	if n := scans.Load(); n != 0 {
		t.Fatalf("the interfaces were scanned %d times", n)
	}
	if out := log.String(); strings.Count(out, "does not let Sol watch the network") != 1 || !strings.Contains(out, "permission denied") {
		t.Fatalf("log: %q", out)
	}
	jump.Store(int64(time.Hour))
	select {
	case r := <-got:
		if r != ReasonWake {
			t.Fatalf("reason %q", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the wake was not noticed")
	}
}

// TestNetlinkUnavailable: any other netlink failure (a sandbox without it) falls back to
// scanning the interfaces.
func TestNetlinkUnavailable(t *testing.T) {
	var scans atomic.Int32
	oldOpen, oldIfs := openNetlink, interfaces
	t.Cleanup(func() { openNetlink, interfaces = oldOpen, oldIfs })
	openNetlink = func() (int, error) { return -1, os.NewSyscallError("socket", syscall.EAFNOSUPPORT) }
	interfaces = func() ([]net.Interface, error) { scans.Add(1); return nil, nil }
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	// Stop Watch and wait for it before the cleanup restores openNetlink and interfaces,
	// which it reads (a race otherwise).
	defer func() { cancel(); <-stopped }()
	go func() {
		defer close(stopped)
		Watch(ctx, Options{PollEvery: 5 * time.Millisecond, Log: slog.New(slog.DiscardHandler)}, func(string) {})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for scans.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatal("no scans")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRelevant: address and link changes on real interfaces and default-route changes
// count; other routes and unknown messages don't.
func TestRelevant(t *testing.T) {
	msg := func(typ uint16, data []byte) syscall.NetlinkMessage {
		return syscall.NetlinkMessage{Header: syscall.NlMsghdr{Type: typ}, Data: data}
	}
	addr := make([]byte, syscall.SizeofIfAddrmsg)
	binary.NativeEndian.PutUint32(addr[4:], 1<<30) // no such interface: counts
	route := make([]byte, syscall.SizeofRtMsg)
	route[4] = syscall.RT_TABLE_MAIN
	other := make([]byte, syscall.SizeofRtMsg)
	other[1], other[4] = 24, syscall.RT_TABLE_MAIN
	cases := []struct {
		m    syscall.NetlinkMessage
		want string
	}{
		{msg(syscall.RTM_NEWADDR, addr), ReasonAddresses},
		{msg(syscall.RTM_DELLINK, make([]byte, syscall.SizeofIfInfomsg)), ReasonInterface},
		{msg(syscall.RTM_NEWROUTE, route), ReasonInterface},
		{msg(syscall.RTM_NEWROUTE, other), ""},
		{msg(syscall.RTM_NEWADDR, addr[:3]), ""},
		{msg(syscall.NLMSG_DONE, nil), ""},
	}
	for i, c := range cases {
		if got := relevant(c.m); got != c.want {
			t.Errorf("case %d: %q, want %q", i, got, c.want)
		}
	}
	// Loopback's changes are noise.
	lo := make([]byte, syscall.SizeofIfAddrmsg)
	binary.NativeEndian.PutUint32(lo[4:], 1)
	if ignoredIndex(1) && relevant(msg(syscall.RTM_NEWADDR, lo)) != "" {
		t.Error("a loopback address change counted")
	}
}
