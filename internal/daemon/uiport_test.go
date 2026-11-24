package daemon

import (
	"log/slog"
	"net"
	"testing"
)

// When something else holds 7777, Sol still starts, on a free port for this run (CI caught a
// daemon that exited instead, after another process took 7777 between setup and start).
func TestListenUIFallsBackWhen7777IsTaken(t *testing.T) {
	hold, err := net.Listen("tcp", "127.0.0.1:7777")
	if err != nil {
		t.Skipf("7777 is in use here already: %v", err)
	}
	defer hold.Close()
	ln, err := listenUI("127.0.0.1:7777", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("want a fallback port, got %v", err)
	}
	defer ln.Close()
	if p := ln.Addr().(*net.TCPAddr).Port; p == 7777 || p == 0 {
		t.Fatalf("port %d", p)
	}
	// Any other taken port is still an error: a port the person chose is not swapped silently.
	busy, _ := net.Listen("tcp", "127.0.0.1:0")
	defer busy.Close()
	if l, err := listenUI(busy.Addr().String(), slog.New(slog.DiscardHandler)); err == nil {
		l.Close()
		t.Fatal("a chosen port that is taken must fail")
	}
}
