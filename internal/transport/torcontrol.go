package transport

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"net/textproto"
	"os"
	"strings"
	"sync"
)

// torControl is a minimal client for Tor's control protocol
// (https://spec.torproject.org/control-spec/). Only the commands Sol needs
// are used: AUTHENTICATE, TAKEOWNERSHIP, GETINFO, ADD_ONION, DEL_ONION, and
// ONION_CLIENT_AUTH_ADD. Asynchronous events are never subscribed to, so
// every reply belongs to the command just sent.
type torControl struct {
	mu   sync.Mutex
	conn net.Conn
	tp   *textproto.Conn
}

// dialTorControl connects to addr and authenticates with the cookie file.
func dialTorControl(ctx context.Context, addr, cookiePath string) (*torControl, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	c := &torControl{conn: conn, tp: textproto.NewConn(conn)}
	cookie, err := os.ReadFile(cookiePath)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("reading tor control cookie: %w", err)
	}
	if _, err := c.do("AUTHENTICATE " + hex.EncodeToString(cookie)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("tor control auth: %w", err)
	}
	// Tor exits when this connection closes, so a crashed sol never leaves
	// a stray tor behind.
	if _, err := c.do("TAKEOWNERSHIP"); err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

// do sends one command and returns the reply lines (without status codes).
// A non-2xx final status becomes an error.
func (c *torControl) do(cmd string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.tp.PrintfLine("%s", cmd); err != nil {
		return nil, err
	}
	var lines []string
	for {
		line, err := c.tp.ReadLine()
		if err != nil {
			return nil, err
		}
		if len(line) < 4 {
			return nil, fmt.Errorf("tor control: short reply %q", line)
		}
		code, sep, rest := line[:3], line[3], line[4:]
		switch sep {
		case '-':
			lines = append(lines, rest)
		case '+':
			data, err := c.tp.ReadDotLines()
			if err != nil {
				return nil, err
			}
			lines = append(lines, rest+strings.Join(data, "\n"))
		case ' ':
			if code[0] != '2' {
				return nil, fmt.Errorf("tor control: %s %s", code, rest)
			}
			if rest != "OK" {
				lines = append(lines, rest)
			}
			return lines, nil
		default:
			return nil, fmt.Errorf("tor control: bad reply %q", line)
		}
	}
}

// getInfo returns the value of one GETINFO key.
func (c *torControl) getInfo(key string) (string, error) {
	lines, err := c.do("GETINFO " + key)
	if err != nil {
		return "", err
	}
	for _, l := range lines {
		if v, ok := strings.CutPrefix(l, key+"="); ok {
			return v, nil
		}
	}
	return "", fmt.Errorf("tor control: GETINFO %s: no value", key)
}

func (c *torControl) close() error { return c.conn.Close() }

// replyValue finds "key=value" in reply lines.
func replyValue(lines []string, key string) string {
	for _, l := range lines {
		if v, ok := strings.CutPrefix(l, key+"="); ok {
			return v
		}
	}
	return ""
}
