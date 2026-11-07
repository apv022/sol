// Command sol runs a Sol node, or pairs with and connects to one.
//
//	sol [node]                      run a node (panel on http://127.0.0.1:8080)
//	sol pair <link> [--name NAME]   pair this device using a link from the panel
//	sol connect [-L svc=addr ...]   open local tunnels to a paired node's services
//	sol audit [--service NAME]      leak checks for anonymous-public services
//	sol version
//
// Run `sol help` for details. See README.md and docs/ for the design.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/apv022/sol/internal/audit"
	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/internal/connect"
	"github.com/apv022/sol/internal/gateway"
	"github.com/apv022/sol/internal/node"
)

// version is set at release time with -ldflags "-X main.version=v0.1.0".
var version = "dev"

const usage = `Sol: self-host private services over Tor. No port forwarding, no accounts.

Usage:
  sol [node] [--data DIR] [--panel ADDR]
        Run a node. Open the panel at http://127.0.0.1:8080 to add services
        and pair devices.

  sol pair <link> [--name NAME] [--client-dir DIR]
        Pair this device with a node using a sol://pair link from its panel.

  sol connect [--node ID] [-L service=127.0.0.1:PORT ...] [--client-dir DIR]
        Open local tunnels to a paired node. Without -L, every service is
        mapped to 127.0.0.1:9000, 9001, ... A sol://pair link may be given
        instead of pairing first.

  sol audit [--service NAME] [--data DIR]
        Run leak checks for anonymous-public services. Exits 1 on failure.

  sol version
`

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()})))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	args := os.Args[1:]
	cmd := "node"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "node":
		err = runNode(ctx, args)
	case "pair":
		err = runPair(ctx, args)
	case "connect":
		err = runConnect(ctx, args)
	case "audit":
		err = runAudit(ctx, args)
	case "gateway":
		err = gateway.Run(ctx)
	case "version":
		fmt.Println("sol", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sol:", err)
		os.Exit(1)
	}
}

func logLevel() slog.Level {
	if os.Getenv("SOL_DEBUG") != "" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

func defaultDataDir() string {
	if d := os.Getenv("SOL_DATA"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".sol"
	}
	return filepath.Join(home, ".sol")
}

func defaultClientDir() string {
	if d := os.Getenv("SOL_CLIENT_DIR"); d != "" {
		return d
	}
	d, err := connect.DefaultDir()
	if err != nil {
		return ".sol-client"
	}
	return d
}

func runNode(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("node", flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory")
	panel := fs.String("panel", "", "panel listen address (default 127.0.0.1:<panel_port>)")
	fs.Parse(args)

	n, err := node.New(node.Options{DataDir: *data, Version: version, PanelAddr: *panel})
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- n.Run(ctx) }()
	select {
	case err := <-done:
		return err
	case <-n.Ready():
	}
	fmt.Printf("Sol node %s\n", n.ID().NodeID())
	fmt.Printf("Panel:  http://%s\n", n.PanelAddr())
	fmt.Printf("Data:   %s (back up node.key)\n", *data)
	fmt.Println("Press Ctrl+C to stop.")
	return <-done
}

func runPair(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	name := fs.String("name", hostname(), "name for this device, shown in the node's panel")
	dir := fs.String("client-dir", defaultClientDir(), "client state directory")
	link, rest := splitPositional(args)
	fs.Parse(rest)
	if link == "" {
		link = fs.Arg(0)
	}
	if link == "" {
		return errors.New("usage: sol pair <sol://pair link> [--name NAME]")
	}
	announcePairing(link)
	c, err := connect.Pair(ctx, *dir, link, *name, clientOptions())
	if err != nil {
		return err
	}
	defer c.Close()
	fmt.Printf("Paired with node %s as device %s.\n", short(c.Node.NodeID), c.Node.DeviceID)
	printServices(c)
	fmt.Println("Run `sol connect` to open tunnels.")
	return nil
}

// tunnelFlags collects repeated -L service=addr flags.
type tunnelFlags []string

func (t *tunnelFlags) String() string     { return strings.Join(*t, ",") }
func (t *tunnelFlags) Set(v string) error { *t = append(*t, v); return nil }

func runConnect(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	nodeID := fs.String("node", "", "node ID (needed only if several nodes are paired)")
	dir := fs.String("client-dir", defaultClientDir(), "client state directory")
	name := fs.String("name", hostname(), "device name, used when a pairing link is given")
	firstPort := fs.Int("port", 9000, "first local port for automatic mapping")
	var tunnels tunnelFlags
	fs.Var(&tunnels, "L", "tunnel `service=127.0.0.1:PORT` (repeatable)")
	link, rest := splitPositional(args)
	fs.Parse(rest)
	if link == "" {
		link = fs.Arg(0)
	}

	var c *connect.Client
	var err error
	if link != "" {
		announcePairing(link)
		c, err = connect.Pair(ctx, *dir, link, *name, clientOptions())
	} else {
		c, err = connect.Load(ctx, *dir, *nodeID, clientOptions())
	}
	if err != nil {
		return err
	}
	defer c.Close()

	fmt.Printf("Connecting to node %s...\n", short(c.Node.NodeID))
	if err := c.WaitReady(ctx); err != nil {
		return err
	}
	if svcs, err := c.Services(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "could not refresh the service list (%v); using the saved one\n", err)
	} else {
		c.Node.Services = svcs
	}

	mapping := map[string]string{}
	if len(tunnels) == 0 {
		port := *firstPort
		for _, s := range c.Node.Services {
			mapping[s.Name] = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
			port++
		}
	}
	for _, t := range tunnels {
		svc, addr, ok := strings.Cut(t, "=")
		if !ok {
			return fmt.Errorf("bad -L %q, want service=127.0.0.1:PORT", t)
		}
		mapping[svc] = addr
		if host, _, err := net.SplitHostPort(addr); err == nil {
			if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
				fmt.Printf("WARNING: %s is reachable from your network (%s). Anyone who can reach this\n"+
					"machine can use the tunnel without pairing. Use this only on a network you trust.\n", svc, addr)
			}
		}
	}
	if len(mapping) == 0 {
		return errors.New("the node has no services yet; add one in its panel")
	}

	errc := make(chan error, len(mapping))
	for svc, addr := range mapping {
		go func() {
			errc <- c.Listen(ctx, addr, svc, func(bound string) {
				scheme := "tcp"
				for _, s := range c.Node.Services {
					if s.Name == svc && s.Protocol == config.ProtocolHTTP {
						scheme = "http"
					}
				}
				hint := ""
				if svc == node.FilesService {
					hint = "  (files: /blob/<sha256>)"
				}
				fmt.Printf("  %-12s -> %s://%s%s\n", svc, scheme, bound, hint)
			})
		}()
	}
	fmt.Println("Tunnels open. Press Ctrl+C to stop.")
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	}
}

func runAudit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("audit", flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory")
	service := fs.String("service", "", "only audit this service")
	fs.Parse(args)
	cfg, err := config.Load(filepath.Join(*data, "config.json"))
	if err != nil {
		return err
	}
	rs := audit.Run(ctx, audit.Env{Config: cfg.Snapshot()}, *service)
	if len(rs) == 0 {
		fmt.Println("No anonymous-public services to audit.")
		return nil
	}
	for _, r := range rs {
		fmt.Printf("%-5s %-14s %-22s %s\n", strings.ToUpper(string(r.Status)), r.Service, r.Check, r.Detail)
	}
	if audit.Failed(rs) {
		os.Exit(1)
	}
	return nil
}

// clientOptions prints connection progress as indented lines.
func clientOptions() connect.Options {
	return connect.Options{
		Transports: connect.DefaultTransports,
		Progress:   func(msg string) { fmt.Println("  " + msg) },
	}
}

func announcePairing(link string) {
	if strings.Contains(link, "tor=") {
		fmt.Println("Pairing over Tor. Starting Tor and finding the node usually takes 30 seconds to 2 minutes...")
	} else {
		fmt.Println("Pairing...")
	}
}

func printServices(c *connect.Client) {
	for _, s := range c.Node.Services {
		fmt.Printf("  service %-12s (%s)\n", s.Name, s.Protocol)
	}
}

// splitPositional lets the link come before or after flags.
func splitPositional(args []string) (string, []string) {
	if len(args) > 0 && strings.HasPrefix(args[0], "sol://") {
		return args[0], args[1:]
	}
	return "", args
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "device"
	}
	return h
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12] + "…"
	}
	return id
}
