//go:build linux

package netwatch

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"syscall"
)

// Netlink multicast groups (linux/rtnetlink.h); the syscall package doesn't name them.
const (
	rtmgrpLink       = 0x1
	rtmgrpIPv4IfAddr = 0x10
	rtmgrpIPv4Route  = 0x40
	rtmgrpIPv6IfAddr = 0x100
	rtmgrpIPv6Route  = 0x400
)

// watchOS subscribes to the kernel's route netlink groups and sends a reason for every
// relevant message until ctx ends. It reports false when netlink is unavailable (some
// sandboxes), so the caller scans instead.
func watchOS(ctx context.Context, send func(string)) bool {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK, syscall.NETLINK_ROUTE)
	if err != nil {
		return false
	}
	sa := &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK,
		Groups: rtmgrpLink | rtmgrpIPv4IfAddr | rtmgrpIPv6IfAddr | rtmgrpIPv4Route | rtmgrpIPv6Route}
	if err := syscall.Bind(fd, sa); err != nil {
		syscall.Close(fd)
		return false
	}
	// Non-blocking, so the runtime poller serves Read and Close interrupts it.
	f := os.NewFile(uintptr(fd), "netlink")
	go func() {
		<-ctx.Done()
		f.Close()
	}()
	go func() {
		buf := make([]byte, 64<<10)
		failures := 0
		for {
			n, err := f.Read(buf)
			if err != nil {
				if ctx.Err() != nil || errors.Is(err, os.ErrClosed) || failures > 8 {
					return
				}
				failures++
				send(ReasonInterface) // ENOBUFS (we fell behind) or the like: something changed
				continue
			}
			failures = 0
			msgs, err := syscall.ParseNetlinkMessage(buf[:n])
			if err != nil {
				continue
			}
			for _, m := range msgs {
				if r := relevant(m); r != "" {
					send(r)
				}
			}
		}
	}()
	return true
}

// relevant says why a netlink message matters ("" when it doesn't): an address or link
// change on a real interface, or a default route coming or going.
func relevant(m syscall.NetlinkMessage) string {
	switch m.Header.Type {
	case syscall.RTM_NEWADDR, syscall.RTM_DELADDR:
		if len(m.Data) < syscall.SizeofIfAddrmsg {
			return ""
		}
		// struct ifaddrmsg: family, prefixlen, flags, scope (u8 each), index (u32).
		if ignoredIndex(int(binary.NativeEndian.Uint32(m.Data[4:8]))) {
			return ""
		}
		return ReasonAddresses
	case syscall.RTM_NEWLINK, syscall.RTM_DELLINK:
		if len(m.Data) < syscall.SizeofIfInfomsg {
			return ""
		}
		// struct ifinfomsg: family, pad (u8), type (u16), index (i32), ...
		if ignoredIndex(int(int32(binary.NativeEndian.Uint32(m.Data[4:8])))) {
			return ""
		}
		return ReasonInterface
	case syscall.RTM_NEWROUTE, syscall.RTM_DELROUTE:
		if len(m.Data) < syscall.SizeofRtMsg {
			return ""
		}
		// struct rtmsg: family, dst_len, src_len, tos, table, ... (u8 each).
		if m.Data[1] != 0 || m.Data[4] != syscall.RT_TABLE_MAIN {
			return "" // not a default route
		}
		return ReasonInterface
	}
	return ""
}

// ignoredIndex reports whether interface index belongs to an ignored interface. A gone
// interface (DELLINK) can't be looked up: it counts.
func ignoredIndex(i int) bool {
	ifc, err := net.InterfaceByIndex(i)
	return err == nil && Ignored(ifc.Name)
}
