package transport

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// dialSOCKS5 connects to target ("host:port") through a SOCKS5 proxy with
// no authentication, sending the hostname to the proxy (so .onion and .i2p
// names are resolved by Tor/i2pd, never by local DNS). Implemented here
// (RFC 1928, CONNECT only) to avoid a dependency.
func dialSOCKS5(ctx context.Context, proxyAddr, target string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 || len(host) > 255 {
		return nil, fmt.Errorf("invalid target %q", target)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("connecting to SOCKS proxy: %w", err)
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	} else {
		conn.SetDeadline(time.Now().Add(2 * time.Minute))
	}
	// Cancel the handshake if ctx is cancelled.
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	fail := func(err error) (net.Conn, error) { conn.Close(); return nil, err }

	// Greeting: version 5, one method, "no auth".
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		return fail(err)
	}
	var resp [2]byte
	if _, err := io.ReadFull(conn, resp[:]); err != nil {
		return fail(err)
	}
	if resp[0] != 5 || resp[1] != 0 {
		return fail(errors.New("SOCKS proxy refused no-auth"))
	}

	// CONNECT with a domain-name address.
	req := []byte{5, 1, 0, 3, byte(len(host))}
	req = append(req, host...)
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := conn.Write(req); err != nil {
		return fail(err)
	}
	var hdr [4]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return fail(err)
	}
	if hdr[1] != 0 {
		return fail(fmt.Errorf("SOCKS connect to %s failed: %s", host, socksError(hdr[1])))
	}
	// Skip the bound address.
	var skip int
	switch hdr[3] {
	case 1:
		skip = 4
	case 4:
		skip = 16
	case 3:
		var l [1]byte
		if _, err := io.ReadFull(conn, l[:]); err != nil {
			return fail(err)
		}
		skip = int(l[0])
	default:
		return fail(errors.New("SOCKS: bad address type in reply"))
	}
	if _, err := io.ReadFull(conn, make([]byte, skip+2)); err != nil {
		return fail(err)
	}
	conn.SetDeadline(time.Time{})
	return conn, nil
}

func socksError(code byte) string {
	switch code {
	case 1:
		return "general failure"
	case 2:
		return "not allowed"
	case 3:
		return "network unreachable"
	case 4:
		return "host unreachable (onion not found or not published yet)"
	case 5:
		return "connection refused"
	case 6:
		return "TTL expired (onion service unreachable)"
	case 0xF0:
		return "onion service descriptor not found"
	case 0xF3:
		return "onion service introduction failed"
	case 0xF4:
		return "onion service rendezvous failed"
	case 0xF5:
		return "missing onion client authorization"
	case 0xF6:
		return "wrong onion client authorization"
	default:
		return fmt.Sprintf("error code %d", code)
	}
}
