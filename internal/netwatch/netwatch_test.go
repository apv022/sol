package netwatch

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestWakeFromSleep: a wall clock that runs ahead of the monotonic one (the machine slept)
// is reported once, after the debounce; a steady clock reports nothing.
func TestWakeFromSleep(t *testing.T) {
	var jump atomic.Int64
	wall := func() time.Time { return time.Now().Round(0).Add(time.Duration(jump.Load())) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan string, 4)
	go Watch(ctx, Options{Debounce: 50 * time.Millisecond, SleepCheckEvery: 20 * time.Millisecond,
		SleepJump: time.Second, PollEvery: time.Hour, Wall: wall, NoOS: true}, func(r string) { got <- r })
	select {
	case r := <-got:
		t.Fatalf("change without one: %q", r)
	case <-time.After(200 * time.Millisecond):
	}
	jump.Store(int64(time.Hour))
	select {
	case r := <-got:
		if r != ReasonWake {
			t.Fatalf("reason %q", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the jump was not noticed")
	}
	select {
	case r := <-got:
		t.Fatalf("a second report for one jump: %q", r)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestStopsWithContext: Watch returns once ctx ends.
func TestStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Watch(ctx, Options{SleepCheckEvery: 10 * time.Millisecond, PollEvery: 10 * time.Millisecond},
			func(string) {})
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not return")
	}
}

func TestIgnoredAndSignature(t *testing.T) {
	for name, want := range map[string]bool{"lo": true, "docker0": true, "veth12": true, "eth0": false, "wlan0": false} {
		if Ignored(name) != want {
			t.Errorf("Ignored(%q) = %v", name, !want)
		}
	}
	if Signature() != Signature() {
		t.Fatal("signature is not stable")
	}
}
