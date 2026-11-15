package node

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/internal/events"
	"github.com/apv022/sol/internal/protocol"
)

// Protocol version compatibility with peers (docs/PROTOCOL.md,
// "Versioning"). A peer whose version range doesn't overlap ours is told
// apart from an unreachable one: the dialer learns the peer's range from the
// first bytes of its hello, which every Sol caller gets, so the callee never
// sends a rejection. The node remembers the verdict per peer, logs it once,
// shows it in the peer view, and redials such a peer only at the slowest
// liveness pace, since only an update on one side fixes it.

// incompatError is a dial failure because the peer speaks no protocol
// version this node does. It unwraps to the *protocol.IncompatibleError.
type incompatError struct {
	msg string
	err *protocol.IncompatibleError
}

func (e *incompatError) Error() string { return e.msg }
func (e *incompatError) Unwrap() error { return e.err }

// incompatText says, for people, which side must update.
func incompatText(name string, e *protocol.IncompatibleError) string {
	if e.PeerOlder() {
		return fmt.Sprintf("%s runs an older Sol (protocol %s; this node needs %d or newer). Update Sol on %s", name, e.Remote, e.Local.Min, name)
	}
	return fmt.Sprintf("%s runs a newer Sol (protocol %s; this node speaks up to %d). Update Sol on this machine", name, e.Remote, e.Local.Max)
}

// noteIncompatible records that p speaks no version we do, logging it only
// when that is news (first time, or the peer's range changed), and returns
// the error the dialer reports.
func (n *Node) noteIncompatible(p config.Peer, err error) error {
	var ie *protocol.IncompatibleError
	if !errors.As(err, &ie) {
		return err
	}
	msg := incompatText(nameOr(p), ie)
	if p.ID != "" {
		n.mu.Lock()
		l := n.linkLocked(p.ID)
		news := l.incompat == nil || *l.incompat != *ie
		cp := *ie
		l.incompat = &cp
		n.mu.Unlock()
		if news {
			n.log.Add(events.KindTransport, msg)
		}
	}
	return &incompatError{msg: msg, err: ie}
}

// incompatLogEvery limits how often an incompatible caller (which is not
// authenticated: its hello comes before any signature) is logged per range.
const incompatLogEvery = time.Hour

// logIncompatibleCaller logs, at most once per incompatLogEvery for each
// version range, that a node running an incompatible Sol tried to connect.
func (n *Node) logIncompatibleCaller(err error) {
	var ie *protocol.IncompatibleError
	if !errors.As(err, &ie) {
		return
	}
	n.mu.Lock()
	if n.incompatSeen == nil {
		n.incompatSeen = map[protocol.Versions]time.Time{}
	}
	last, seen := n.incompatSeen[ie.Remote]
	fresh := !seen || time.Since(last) >= incompatLogEvery
	if fresh {
		n.incompatSeen[ie.Remote] = time.Now()
	}
	n.mu.Unlock()
	if !fresh {
		slog.Debug("incompatible caller", "err", err)
		return
	}
	if ie.PeerOlder() {
		n.log.Add(events.KindReject, fmt.Sprintf("a node running an older Sol (protocol %s) tried to connect; this node needs %d or newer. Update Sol there", ie.Remote, ie.Local.Min))
	} else {
		n.log.Add(events.KindReject, fmt.Sprintf("a node running a newer Sol (protocol %s) tried to connect; this node speaks up to %d. Update Sol on this machine", ie.Remote, ie.Local.Max))
	}
}
