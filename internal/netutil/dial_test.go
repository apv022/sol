package netutil

import (
	"net"
	"strconv"
	"testing"
	"time"
)

func listenPort(t *testing.T, addr string) (int, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("can't listen on %s: %v", addr, err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, func() { ln.Close() }
}

func TestDialLocalIPv4(t *testing.T) {
	port, stop := listenPort(t, "127.0.0.1:0")
	defer stop()
	c, err := DialLocal(port, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

// Regression: an app bound only to [::1] (a "localhost" Node/Bun server) was
// reported as not running and its peers got an empty reply.
func TestDialLocalIPv6Only(t *testing.T) {
	port, stop := listenPort(t, "[::1]:0")
	defer stop()
	if c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 200*time.Millisecond); err == nil {
		c.Close()
		t.Skip("something else answers on 127.0.0.1 at this port")
	}
	c, err := DialLocal(port, time.Second)
	if err != nil {
		t.Fatalf("an app on [::1] only must be reachable: %v", err)
	}
	if !c.RemoteAddr().(*net.TCPAddr).IP.IsLoopback() {
		t.Fatalf("dialled a non-loopback address: %v", c.RemoteAddr())
	}
	c.Close()
}

func TestDialLocalNothingListening(t *testing.T) {
	port, stop := listenPort(t, "127.0.0.1:0")
	stop()
	if c, err := DialLocal(port, 300*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("expected an error with nothing listening")
	}
}
