// Package netwatch tells the daemon when this machine's network may have changed or it woke
// from sleep, so sessions can be checked and redialed at once instead of after a timeout
// (design/PROTOCOL.md §Liveness and healing). Ported from the PoC's liveness supervisor
// (~/solbak/internal/node/liveness.go) and Tor health watcher.
//
// Sources:
//
//   - The wall clock against the monotonic clock: the monotonic clock stops while the
//     machine sleeps (Linux, Android, macOS), the wall clock doesn't, so a gap between them
//     means it slept. Works everywhere.
//   - On Linux and Android, a netlink route socket: address, link and default-route changes
//     arrive as they happen.
//   - Elsewhere (or when netlink is unavailable), a scan of the interface addresses every
//     PollEvery: the only portable way without cgo.
//   - Where the OS forbids watching (Android's SELinux policy denies apps a bound netlink
//     route socket, and with it net.Interfaces), neither: retrying or scanning would only
//     be refused every few seconds. The platform's own callback reports changes instead
//     (the Android shell's network callback runs `sol network-changed`), and the clock
//     still catches wake-ups. Said once in the log.
//
// Invariants: bursts collapse into one call after Debounce; the callback never runs
// concurrently with itself; nothing runs after ctx ends. Virtual interfaces (containers,
// bridges) and loopback are ignored.
package netwatch

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"sort"
	"strings"
	"time"
)

// Options tune a Watch. The zero value of any field means its default (in brackets).
type Options struct {
	// Debounce collapses a burst of changes into one callback [2s].
	Debounce time.Duration
	// SleepCheckEvery and SleepJump: the wall clock is compared with the monotonic clock
	// this often; drifting apart by more than SleepJump means the machine slept [5s, 20s].
	SleepCheckEvery, SleepJump time.Duration
	// PollEvery is the address scan interval where the OS can't push changes [5s].
	PollEvery time.Duration
	// Wall reads the wall clock without its monotonic reading (a test hook)
	// [time.Now().Round(0)].
	Wall func() time.Time
	// NoOS turns off the OS change source (tests): only the clock and the scan run.
	NoOS bool
	// Log says once when the OS forbids watching the network [slog's default].
	Log *slog.Logger
}

func (o Options) withDefaults() Options {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&o.Debounce, 2*time.Second)
	def(&o.SleepCheckEvery, 5*time.Second)
	def(&o.SleepJump, 20*time.Second)
	def(&o.PollEvery, 5*time.Second)
	if o.Wall == nil {
		o.Wall = func() time.Time { return time.Now().Round(0) }
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return o
}

// Reasons passed to the callback (joined with ", " when a burst has several).
const (
	ReasonWake      = "resumed from sleep"
	ReasonInterface = "network interfaces changed"
	ReasonAddresses = "addresses changed"
)

// Watch calls onChange with the reasons of each debounced burst of changes until ctx ends.
// It blocks; run it in its own goroutine.
func Watch(ctx context.Context, o Options, onChange func(reason string)) {
	o = o.withDefaults()
	kick := make(chan string, 16)
	send := func(r string) {
		select {
		case kick <- r:
		default: // a burst: one is pending already
		}
	}
	scan := o.NoOS
	if !scan {
		err := watchOS(ctx, send)
		scan = err != nil
		if errors.Is(err, os.ErrPermission) {
			scan = false
			o.Log.Info("the OS does not let Sol watch the network; relying on the platform's network callback (sol network-changed) and the sleep detector", "err", err.Error())
		}
	}
	var lastSig string
	var poll <-chan time.Time
	if scan {
		var err error
		if lastSig, err = signature(); errors.Is(err, os.ErrPermission) {
			o.Log.Info("the OS does not let Sol read the network interfaces; relying on the platform's network callback (sol network-changed) and the sleep detector", "err", err.Error())
		} else {
			t := time.NewTicker(o.PollEvery)
			defer t.Stop()
			poll = t.C
		}
	}
	clock := time.NewTicker(o.SleepCheckEvery)
	defer clock.Stop()
	lastMono, lastWall := time.Now(), o.Wall()
	var debounce <-chan time.Time
	var reasons []string
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-kick:
			if len(reasons) < 8 && !contains(reasons, r) {
				reasons = append(reasons, r)
			}
			if debounce == nil {
				debounce = time.After(o.Debounce)
			}
		case <-debounce:
			debounce = nil
			onChange(strings.Join(reasons, ", "))
			reasons = nil
		case <-poll:
			if sig, _ := signature(); sig != lastSig {
				lastSig = sig
				send(ReasonAddresses)
			}
		case <-clock.C:
			mono, wall := time.Now(), o.Wall()
			if skew := wall.Sub(lastWall) - mono.Sub(lastMono); skew > o.SleepJump || skew < -o.SleepJump {
				send(ReasonWake)
			}
			lastMono, lastWall = mono, wall
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ignoredPrefixes name interfaces whose changes don't move this machine between networks.
var ignoredPrefixes = []string{"lo", "veth", "docker", "br-", "virbr", "cni", "flannel"}

// Ignored reports whether changes on interface name are noise.
func Ignored(name string) bool {
	for _, p := range ignoredPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// Signature summarises the machine's usable network addresses ("" if they can't be read).
// A change means routes probably changed too.
func Signature() string {
	s, _ := signature()
	return s
}

// interfaces lists the network interfaces (a test hook).
var interfaces = net.Interfaces

// signature is Signature with the reason it could not read the interfaces.
func signature() (string, error) {
	ifs, err := interfaces()
	if err != nil {
		return "", err
	}
	var parts []string
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || Ignored(ifc.Name) {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() {
				continue
			}
			parts = append(parts, ifc.Name+"="+ipn.IP.String())
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ","), nil
}
