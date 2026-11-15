package node

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apv022/sol/internal/control"
	"github.com/apv022/sol/internal/transport"
)

// Regression: on macOS a long data directory made the control socket fail
// with a cryptic "bind: invalid argument". It must fail clearly instead, on
// every OS, before reaching the OS limit.
func TestControlSocketPathTooLong(t *testing.T) {
	long := filepath.Join("/tmp", strings.Repeat("d", 100), "control.sock")
	if _, err := listenControl(long); err == nil || !strings.Contains(err.Error(), "shorter data directory") {
		t.Fatalf("long socket path: %v", err)
	}
}

// watcherTransport records NetworkChanged calls.
type watcherTransport struct {
	transport.Transport
	got chan string
}

func (w *watcherTransport) Name() string                 { return "watcher" }
func (w *watcherTransport) NetworkChanged(reason string) { w.got <- reason }

// `sol network-changed [reason]` must reach the transports with a default
// reason, a length cap, and no control characters.
func TestControlNetworkChanged(t *testing.T) {
	w := &watcherTransport{got: make(chan string, 4)}
	reg := transport.NewRegistry()
	reg.Add(w)
	n := &Node{reg: reg, netKick: make(chan string, 8), ctx: context.Background()}
	for _, tc := range []struct{ in, want string }{
		{"", "requested"},
		{"wifi to cellular", "wifi to cellular"},
		{"a\nb\x00c", "abc"},
		{strings.Repeat("x", 500), strings.Repeat("x", control.MaxReasonLen)},
	} {
		client, server := net.Pipe()
		go n.handleControl(server)
		client.SetDeadline(time.Now().Add(5 * time.Second))
		if err := json.NewEncoder(client).Encode(control.Request{Op: control.OpNetworkChanged, Name: tc.in}); err != nil {
			t.Fatalf("%q: %v", tc.in, err)
		}
		var resp control.Response
		if err := json.NewDecoder(client).Decode(&resp); err != nil || !resp.OK {
			t.Fatalf("%q: response %+v, %v", tc.in, resp, err)
		}
		client.Close()
		select {
		case got := <-w.got:
			if got != tc.want {
				t.Errorf("reason %q: transport got %q, want %q", tc.in, got, tc.want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("reason %q: transport never told", tc.in)
		}
	}
}
