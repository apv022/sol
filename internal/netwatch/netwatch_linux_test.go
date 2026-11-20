//go:build linux

package netwatch

import (
	"encoding/binary"
	"syscall"
	"testing"
)

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
