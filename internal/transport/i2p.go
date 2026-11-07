package transport

import (
	"context"
	"net"

	"github.com/apv022/sol/internal/runtime"
)

// I2P is the I2P transport via i2pd. It is registered so the panel, config,
// and pairing links already know about it, but the MVP does not implement it.
//
// Implementation plan (see docs/EXTENDING.md, "Adding a transport"):
//   - Start: runtime.Start an i2pd Program with a generated i2pd.conf
//     (SAM enabled, HTTP/SOCKS proxies on free ports), wait for SAM.
//   - Publish: create a persistent SAM STREAM session per publication ID with
//     a stored destination key; forward accepted streams to p.Target. Return
//     the .b32.i2p address. Or, simpler: write a server tunnel to tunnels.conf
//     and reload i2pd.
//   - Dial: SOCKS5 via i2pd's SOCKS proxy (reuse dialSOCKS5 from socks.go).
//   - ClientAuth: encrypted leasesets where i2pd supports them.
type I2P struct {
	DataDir  string
	Provider runtime.Provider
}

// NewI2P constructs the transport.
func NewI2P(dataDir string, provider runtime.Provider) *I2P {
	return &I2P{DataDir: dataDir, Provider: provider}
}

func (i *I2P) Name() string { return "i2p" }

func (i *I2P) Start(ctx context.Context) error { return ErrNotImplemented }

func (i *I2P) Publish(ctx context.Context, p Publication) (string, error) {
	return "", ErrNotImplemented
}

func (i *I2P) Unpublish(id string) error { return nil }

func (i *I2P) Handles(addr string) bool { return IsI2P(addr) }

func (i *I2P) Dial(ctx context.Context, addr string) (net.Conn, error) {
	return nil, ErrNotImplemented
}

func (i *I2P) Status() Status {
	return Status{Name: i.Name(), State: StateFailed, Detail: "I2P support is not implemented yet (roadmap)"}
}

func (i *I2P) Stop() error { return nil }
