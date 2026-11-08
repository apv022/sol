// Command sol runs a Sol node and talks to it.
//
//	sol                              run the node (panel on http://127.0.0.1:8080)
//	sol invite                       print a pairing link for another machine
//	sol pair <link>                  pair with the node that made the link
//	sol peers [-r]                   list peers and what they share
//	sol share <peer> <service>       let a peer open a service (--off to stop)
//	sol open <peer> [-L svc=addr]    local tunnels to a peer's services
//	sol send <peer> <file>...        send files to a peer's inbox
//	sol unpair <peer>                remove a peer (and tell it)
//	sol name [NEW]                   show or change this node's name
//	sol audit, sol version
//
// Every command except running the node talks to the running node over its
// control socket. Run `sol help` for details. See README.md and docs/PEERS.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/apv022/sol/internal/audit"
	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/internal/control"
	"github.com/apv022/sol/internal/gateway"
	"github.com/apv022/sol/internal/netutil"
	"github.com/apv022/sol/internal/node"
	"github.com/apv022/sol/internal/peers"
)

// version is set at release time with -ldflags "-X main.version=v0.1.0".
var version = "dev"

const usage = `Sol: share your apps and files between your own machines over Tor.
No port forwarding, no accounts.

Run the node (keep it running; everything else talks to it):
  sol [run] [--data DIR] [--panel ADDR]
        Panel at http://127.0.0.1:8080.

Pair two machines (once):
  sol invite                  on machine A: prints a link (works once, for 10 minutes)
  sol pair '<link>'           on machine B: pairs B with A. Both are now peers.

Use your peers:
  sol peers [-r]              list peers; -r asks each one what it shares now
  sol share <peer> <service>  let a peer open one of your services (--off to stop).
                              "_files" is your shared-files page.
  sol open <peer> [-L service=127.0.0.1:PORT ...] [--port 9000]
                              open local tunnels to what a peer shares with you
  sol send <peer> <file>...   send files to a peer (they land in its Files tab)
  sol unpair <peer>           remove a peer here and tell it to remove you

Other:
  sol name [NEW]              show or change this node's name, as peers see it
  sol status                  this node's id, name and addresses
  sol audit [--service NAME]  leak checks for anonymous-public services
  sol version

<peer> is a name or the start of an id. Add --data DIR to any command to
use a node other than ~/.sol (or set SOL_DATA).
`

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()})))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	args := os.Args[1:]
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	commands := map[string]func(context.Context, []string) error{
		"run": runNode, "node": runNode,
		"invite": runInvite, "pair": runPair, "peers": runPeers, "share": runShare,
		"open": runOpen, "send": runSend, "unpair": runUnpair, "name": runName,
		"status": runStatus, "audit": runAudit,
		"gateway": func(ctx context.Context, _ []string) error { return gateway.Run(ctx) },
	}
	var err error
	switch cmd {
	case "version":
		fmt.Println("sol", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	case "connect":
		err = errors.New("`sol connect` is gone: every machine now runs `sol` and pairs as a peer. See `sol help` (`sol pair`, then `sol open`)")
	default:
		f, ok := commands[cmd]
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
			os.Exit(2)
		}
		err = f(ctx, args)
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

// flags parses args with a --data flag added, accepting positional
// arguments before or after the flags.
func flags(name string, args []string, setup func(*flag.FlagSet)) (data string, pos []string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	d := fs.String("data", defaultDataDir(), "node data directory")
	if setup != nil {
		setup(fs)
	}
	for len(args) > 0 {
		fs.Parse(args)
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
	return *d, pos
}

func sock(data string) string { return control.Path(data) }

func runNode(ctx context.Context, args []string) error {
	var panel string
	data, _ := flags("run", args, func(fs *flag.FlagSet) {
		fs.StringVar(&panel, "panel", "", "panel listen address (default 127.0.0.1:<panel_port>)")
	})
	n, err := node.New(node.Options{DataDir: data, Version: version, PanelAddr: panel})
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
	fmt.Printf("Sol node %q (%s)\n", n.Name(), short(n.ID().NodeID()))
	fmt.Printf("Panel:  http://%s\n", n.PanelAddr())
	fmt.Printf("Data:   %s (back up node.key: it is this node's identity)\n", data)
	fmt.Println("Pair another machine with `sol invite`. Press Ctrl+C to stop.")
	return <-done
}

func runStatus(ctx context.Context, args []string) error {
	data, _ := flags("status", args, nil)
	var st control.Status
	if err := control.Call(ctx, sock(data), control.Request{Op: control.OpStatus}, &st); err != nil {
		return err
	}
	printStatus(st)
	return nil
}

func printStatus(st control.Status) {
	fmt.Printf("Name:   %s\nNode:   %s\n", st.Name, st.NodeID)
	if st.Panel != "" {
		fmt.Printf("Panel:  http://%s\n", st.Panel)
	}
	for _, t := range sortedKeys(st.Addrs) {
		fmt.Printf("%-7s %s\n", t+":", st.Addrs[t])
	}
}

func runName(ctx context.Context, args []string) error {
	data, pos := flags("name", args, nil)
	var st control.Status
	req := control.Request{Op: control.OpName, Name: strings.Join(pos, " ")}
	if err := control.Call(ctx, sock(data), req, &st); err != nil {
		return err
	}
	fmt.Println(st.Name)
	return nil
}

func runInvite(ctx context.Context, args []string) error {
	data, _ := flags("invite", args, nil)
	var inv control.Invite
	if err := control.Call(ctx, sock(data), control.Request{Op: control.OpInvite}, &inv); err != nil {
		return err
	}
	fmt.Println("On the other machine (running `sol`), run:")
	fmt.Printf("\n  sol pair '%s'\n\n", inv.Link)
	fmt.Println("The link works once and expires in 10 minutes. Treat it like a password.")
	fmt.Println("(The panel's Peers tab shows it as a QR code too.)")
	return nil
}

func runPair(ctx context.Context, args []string) error {
	data, pos := flags("pair", args, nil)
	if len(pos) != 1 {
		return errors.New("usage: sol pair '<sol://pair link>'")
	}
	if strings.Contains(pos[0], "tor=") {
		fmt.Println("Pairing over Tor. Finding the other node usually takes 30 seconds to 2 minutes...")
	} else {
		fmt.Println("Pairing...")
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	var p control.PeerView
	if err := control.Call(ctx, sock(data), control.Request{Op: control.OpPair, Link: pos[0]}, &p); err != nil {
		return err
	}
	fmt.Printf("Paired with %s (%s).\n", p.Name, p.ID)
	printShared(p)
	fmt.Printf("It can send you files. Share more with `sol share %s <service>`.\n", quote(p.Name))
	return nil
}

func printShared(p control.PeerView) {
	var names []string
	for _, s := range p.Services {
		if s.Name != peers.Inbox {
			names = append(names, label(s.Name))
		}
	}
	if len(names) == 0 {
		fmt.Printf("  %s shares nothing with you yet (besides its inbox).\n", p.Name)
	} else {
		fmt.Printf("  %s shares: %s. Open with `sol open %s`.\n", p.Name, strings.Join(names, ", "), quote(p.Name))
	}
}

func runPeers(ctx context.Context, args []string) error {
	var refresh bool
	data, pos := flags("peers", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&refresh, "r", false, "ask each peer what it shares now (slow over Tor)")
	})
	req := control.Request{Op: control.OpPeers, Refresh: refresh}
	if len(pos) > 0 {
		req.Peer = pos[0]
	}
	if refresh {
		ctx2, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		ctx = ctx2
	}
	var list []control.PeerView
	if err := control.Call(ctx, sock(data), req, &list); err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("No peers yet. Run `sol invite` here and `sol pair '<link>'` on the other machine.")
		return nil
	}
	for _, p := range list {
		state := "offline"
		if p.Online {
			state = "online"
		}
		fmt.Printf("%s  (%s, %s)\n", p.Name, p.ID, state)
		var theirs []string
		for _, s := range p.Services {
			theirs = append(theirs, label(s.Name))
		}
		var mine []string
		for _, s := range p.Shares {
			mine = append(mine, label(s))
		}
		fmt.Printf("  shares with you: %s\n", listOrNone(theirs))
		fmt.Printf("  you share:       %s\n", listOrNone(mine))
		if p.Error != "" {
			fmt.Printf("  couldn't refresh: %s\n", p.Error)
		}
	}
	return nil
}

func runShare(ctx context.Context, args []string) error {
	var off bool
	data, pos := flags("share", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&off, "off", false, "stop sharing instead")
	})
	if len(pos) != 2 {
		return errors.New("usage: sol share <peer> <service> [--off]")
	}
	var p control.PeerView
	req := control.Request{Op: control.OpShare, Peer: pos[0], Service: pos[1], Shared: !off}
	if err := control.Call(ctx, sock(data), req, &p); err != nil {
		return err
	}
	if off {
		fmt.Printf("%s can no longer open %s.\n", p.Name, label(pos[1]))
	} else {
		fmt.Printf("%s can now open %s (it runs `sol open %s` there).\n", p.Name, label(pos[1]), quote(nameOf(ctx, data)))
	}
	return nil
}

// tunnelFlags collects repeated -L service=addr flags.
type tunnelFlags []string

func (t *tunnelFlags) String() string     { return strings.Join(*t, ",") }
func (t *tunnelFlags) Set(v string) error { *t = append(*t, v); return nil }

func runOpen(ctx context.Context, args []string) error {
	var tunnels tunnelFlags
	var firstPort int
	data, pos := flags("open", args, func(fs *flag.FlagSet) {
		fs.Var(&tunnels, "L", "tunnel `service=127.0.0.1:PORT` (repeatable)")
		fs.IntVar(&firstPort, "port", 9000, "first local port for automatic mapping")
	})
	if len(pos) != 1 {
		return errors.New("usage: sol open <peer> [-L service=127.0.0.1:PORT ...]")
	}
	path := sock(data)
	fmt.Println("Asking the peer what it shares (over Tor this can take a minute)...")
	rctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	var list []control.PeerView
	err := control.Call(rctx, path, control.Request{Op: control.OpPeers, Peer: pos[0], Refresh: true}, &list)
	cancel()
	if err != nil {
		return err
	}
	p := list[0]
	if p.Error != "" {
		fmt.Fprintf(os.Stderr, "couldn't refresh (%s); using the last known list\n", p.Error)
	}

	mapping := map[string]string{}
	if len(tunnels) == 0 {
		port := firstPort
		for _, s := range p.Services {
			if s.Name == peers.Inbox {
				continue // use `sol send` for that
			}
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
		return fmt.Errorf("%s doesn't share anything with you yet. On %s: sol share %s <service>", p.Name, p.Name, quote(nameOf(ctx, data)))
	}

	errc := make(chan error, len(mapping))
	for _, svc := range sortedKeys(mapping) {
		ln, err := net.Listen("tcp", mapping[svc])
		if err != nil {
			return err
		}
		defer ln.Close()
		scheme := "tcp"
		for _, s := range p.Services {
			if s.Name == svc && s.Protocol == config.ProtocolHTTP {
				scheme = "http"
			}
		}
		fmt.Printf("  %-14s -> %s://%s\n", label(svc), scheme, ln.Addr())
		go func() { errc <- tunnel(ctx, ln, path, p.ID, svc) }()
	}
	fmt.Println("Tunnels open. Press Ctrl+C to stop.")
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	}
}

// tunnel accepts local connections and pipes each one to the peer's
// service through the node.
func tunnel(ctx context.Context, ln net.Listener, path, peer, service string) error {
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		local, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func() {
			c, err := control.Dial(ctx, path)
			if err != nil {
				local.Close()
				return
			}
			err = c.Send(control.Request{Op: control.OpDial, Peer: peer, Service: service})
			if err == nil {
				err = c.Recv(nil) // the node's answer: did the peer accept?
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", label(service), err)
				c.Close()
				local.Close()
				return
			}
			netutil.Pipe(local, c)
		}()
	}
}

func runSend(ctx context.Context, args []string) error {
	data, pos := flags("send", args, nil)
	if len(pos) < 2 {
		return errors.New("usage: sol send <peer> <file> [more files]")
	}
	for _, file := range pos[1:] {
		if err := sendOne(ctx, sock(data), pos[0], file); err != nil {
			return err
		}
	}
	return nil
}

func sendOne(ctx context.Context, path, peer, file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.IsDir() {
		return fmt.Errorf("%s is a folder; send its files (or a .zip/.tar of it)", file)
	}
	c, err := control.Dial(ctx, path)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Send(control.Request{Op: control.OpSend, Peer: peer, Name: filepath.Base(file)}); err != nil {
		return err
	}
	if err := c.Recv(nil); err != nil {
		return err
	}
	fmt.Printf("Sending %s (%s)...\n", filepath.Base(file), human(st.Size()))
	if _, err := io.Copy(c.Conn, f); err != nil {
		return err
	}
	c.CloseWrite()
	var sent control.Sent
	if err := c.Recv(&sent); err != nil {
		return err
	}
	fmt.Printf("  delivered and verified, SHA-256 %s\n", sent.Hash)
	return nil
}

func runUnpair(ctx context.Context, args []string) error {
	data, pos := flags("unpair", args, nil)
	if len(pos) != 1 {
		return errors.New("usage: sol unpair <peer>")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var u control.Unpaired
	if err := control.Call(ctx, sock(data), control.Request{Op: control.OpUnpair, Peer: pos[0]}, &u); err != nil {
		return err
	}
	if u.Notified {
		fmt.Printf("Unpaired from %s, and %s removed you too.\n", u.Name, u.Name)
	} else {
		fmt.Printf("Removed %s. It couldn't be reached to tell it, but it can no longer reach you.\n", u.Name)
	}
	return nil
}

func runAudit(ctx context.Context, args []string) error {
	var service string
	data, _ := flags("audit", args, func(fs *flag.FlagSet) {
		fs.StringVar(&service, "service", "", "only audit this service")
	})
	cfg, err := config.Load(filepath.Join(data, "config.json"))
	if err != nil {
		return err
	}
	rs := audit.Run(ctx, audit.Env{Config: cfg.Snapshot()}, service)
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

// nameOf returns this node's name (for hints), or "<this node>".
func nameOf(ctx context.Context, data string) string {
	var st control.Status
	if control.Call(ctx, sock(data), control.Request{Op: control.OpStatus}, &st) != nil {
		return "<this node>"
	}
	return st.Name
}

func label(service string) string {
	switch service {
	case peers.Files:
		return "shared files"
	case peers.Inbox:
		return "inbox"
	}
	return service
}

func listOrNone(xs []string) string {
	if len(xs) == 0 {
		return "nothing"
	}
	return strings.Join(xs, ", ")
}

func quote(s string) string {
	if strings.ContainsAny(s, " '\"") {
		return strconv.Quote(s)
	}
	return s
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func human(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	}
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12] + "…"
	}
	return id
}
