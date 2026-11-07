// Package gateway will hold `sol gateway` (v0.2): an HTTPS front door that
// lets plain browsers reach a node's services with nothing installed.
//
// Design (docs/spec.md 7.4), already supported by the rest of the code:
//   - The gateway is itself a paired device of the node.
//   - Per browser it creates a session keypair, stored server-side, and
//     pairs it as a device of kind config.DeviceKindGatewaySession, so every
//     browser is listed and revocable individually in the node's panel.
//   - For each browser request it opens a connect.Session to the node and
//     proxies the stream, exactly as `sol connect` does.
//   - The gateway can read what it proxies, so the recommended setup is a
//     gateway the user runs themselves, on a different machine and network
//     from the origin.
package gateway

import (
	"context"
	"errors"
)

// ErrNotImplemented is returned until v0.2.
var ErrNotImplemented = errors.New("gateway mode ships in v0.2; see docs/spec.md section 7.4")

// Run starts the gateway. Not implemented yet.
func Run(ctx context.Context) error { return ErrNotImplemented }
