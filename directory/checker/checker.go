// Command checker will probe each host in directory/hosts.json over its
// transports and record an up/slow/down status. It is a standalone tool for
// whoever maintains the known-hosts directory, not part of the sol binary.
//
// Planned behaviour (docs/spec.md section 9): for each address, time a GET
// (clearnet directly, .onion and .i2p through a local SOCKS proxy), classify
// it as up (<2 s), slow (<10 s) or down, and rewrite hosts.json atomically,
// changing only the status fields.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "checker: not implemented yet (roadmap, see docs/spec.md section 9)")
	os.Exit(1)
}
