// Command sol is the Sol daemon, its CLI, and the first-party apps in one binary.
//
//	sol init [--name N --device D | --join] [--listen ADDR] [--transport NAME] [--ui ADDR] [--files DIR]
//	sol run                      run this device in the foreground
//	sol status                   who I am, my endpoints and sessions
//	sol invite [--join]          print a one-time pair (or join) link
//	sol pair <link>              redeem a pair link, or a join link on a fresh --join device
//	sol apps                     list installed apps and their UI addresses
//	sol app install <dir>        install (or replace) the app in dir
//	sol app remove <id>          remove an app (its data stays)
//	sol app run <id>             run a builtin app in the foreground (the daemon does this)
//
// Every command takes --dir (default $SOL_DIR or ~/.sol). The logic lives in internal/daemon;
// this file only parses arguments and prints.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/apv022/sol/internal/apphost"
	"github.com/apv022/sol/internal/builtins"
	"github.com/apv022/sol/internal/daemon"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sol:", err)
		os.Exit(1)
	}
}

const usage = `usage: sol <command> [flags]

  init [--name N --device D | --join] [--listen ADDR] [--transport NAME] [--ui ADDR] [--files DIR]
  run
  status [--json]
  invite [--join]
  pair <link>
  apps [--json]
  app install <dir> | app remove <id> | app run <id>

Every command takes --dir DIR (default $SOL_DIR or ~/.sol).`

func run(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Println(usage)
		return nil
	}
	cmd, rest := args[0], args[1:]
	if cmd == "app" {
		if len(rest) == 0 {
			return errors.New(usage)
		}
		cmd, rest = "app "+rest[0], rest[1:]
	}
	fs := flag.NewFlagSet("sol "+cmd, flag.ContinueOnError)
	dir := fs.String("dir", daemon.DefaultDir(), "data directory")
	asJSON := fs.Bool("json", false, "print JSON")
	switch cmd {
	case "init":
		name := fs.String("name", "", "your name (new person)")
		device := fs.String("device", hostname(), "this device's name")
		join := fs.Bool("join", false, "this device will join an existing person (then: sol pair <join link>)")
		listen := fs.String("listen", "", "transport listen address (default: a free port on 0.0.0.0)")
		tr := fs.String("transport", "direct", "transport: direct, clearnet, wg or tor")
		ui := fs.String("ui", "127.0.0.1:7777", "address of the app UIs")
		files := fs.String("files", "", "the synced folder (default ~/SolDrive)")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		cfg := daemon.Config{UI: *ui, Files: absOrEmpty(*files)}
		if *listen != "" || *tr != "direct" {
			cfg.Transports = []daemon.TransportConfig{{Name: *tr, Listen: *listen}}
		}
		if !*join && *name == "" {
			return errors.New("init: --name is required (or --join for a device joining an existing person)")
		}
		if err := daemon.Init(*dir, daemon.InitOptions{Name: *name, Device: *device, Join: *join, Config: cfg,
			Builtins: builtins.All()}); err != nil {
			return err
		}
		if *join {
			fmt.Printf("Device created in %s. Start it with `sol run`, then `sol pair <join link>` (from `sol invite --join` on your other device).\n", *dir)
		} else {
			fmt.Printf("Sol is set up for %s in %s. Start it with `sol run`.\n", *name, *dir)
		}
		return nil
	case "run":
		if err := fs.Parse(rest); err != nil {
			return err
		}
		return runDaemon(*dir)
	case "app run":
		if err := fs.Parse(rest); err != nil || fs.NArg() != 1 {
			return errors.New("usage: sol app run <id>")
		}
		b, ok := builtins.All()[fs.Arg(0)]
		if !ok {
			return fmt.Errorf("no builtin app %q", fs.Arg(0))
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return b.Run(ctx, apphost.EnvFromOS())
	}
	inviteJoin := fs.Bool("join", false, "invite: a join link (add a device of mine) instead of a pair link")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	c, err := daemon.Connect(*dir)
	if err != nil {
		return err
	}
	ctx := context.Background()
	switch cmd {
	case "status":
		var st daemon.Status
		if err := c.Do(ctx, "GET", "/admin/v1/status", nil, &st); err != nil {
			return err
		}
		if *asJSON {
			return printJSON(st)
		}
		fmt.Printf("person   %s (%s)\ndevice   %s (%s)\nui       %s\nfiles    %s\n", st.Name, st.Person, st.DevName, st.Device, st.UI, st.Files)
		for _, e := range st.Endpoints {
			fmt.Printf("endpoint %s\n", e)
		}
		for _, s := range st.Sessions {
			fmt.Printf("session  %s %s (person %s)\n", s.Relation, s.Device, s.Person)
		}
		return nil
	case "invite":
		var out struct{ Link string }
		if err := c.Do(ctx, "POST", "/admin/v1/invite", map[string]bool{"join": *inviteJoin}, &out); err != nil {
			return err
		}
		fmt.Println(out.Link)
		return nil
	case "pair":
		if fs.NArg() != 1 {
			return errors.New("usage: sol pair <link>")
		}
		var out struct{ Kind, Person string }
		if err := c.Do(ctx, "POST", "/admin/v1/pair", map[string]string{"link": fs.Arg(0)}, &out); err != nil {
			return err
		}
		if out.Kind == "join" {
			fmt.Println("Joined. This device now belongs to", out.Person)
		} else {
			fmt.Println("Paired with", out.Person)
		}
		return nil
	case "apps":
		var apps []daemon.AppInfo
		if err := c.Do(ctx, "GET", "/admin/v1/apps", nil, &apps); err != nil {
			return err
		}
		if *asJSON {
			return printJSON(apps)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tVERSION\tRUNNING\tUI")
		for _, a := range apps {
			fmt.Fprintf(tw, "%s\t%s\t%v\t%s\n", a.ID, a.Version, a.Running, a.URL)
		}
		return tw.Flush()
	case "app install":
		if fs.NArg() != 1 {
			return errors.New("usage: sol app install <dir>")
		}
		var m struct{ ID, Version string }
		if err := c.Do(ctx, "POST", "/admin/v1/apps/install", map[string]string{"dir": absOrEmpty(fs.Arg(0))}, &m); err != nil {
			return err
		}
		fmt.Printf("Installed %s %s\n", m.ID, m.Version)
		return nil
	case "app remove":
		if fs.NArg() != 1 {
			return errors.New("usage: sol app remove <id>")
		}
		if err := c.Do(ctx, "POST", "/admin/v1/apps/remove", map[string]string{"id": fs.Arg(0)}, nil); err != nil {
			return err
		}
		fmt.Println("Removed", fs.Arg(0))
		return nil
	}
	return fmt.Errorf("unknown command %q\n%s", cmd, usage)
}

// runDaemon is `sol run`: start, print where things are, wait for a signal, stop.
func runDaemon(dir string) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	d, err := daemon.Start(daemon.Options{Dir: dir, Builtins: builtins.All(), Log: log})
	if err != nil {
		return err
	}
	st := d.Status()
	fmt.Printf("Sol is running for %s on %s.\n  apps:     %s\n  endpoints: %s\nCtrl-C to stop.\n", st.Name, st.DevName, st.UI, strings.Join(st.Endpoints, ", "))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	return d.Close()
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func absOrEmpty(p string) string {
	if p == "" {
		return ""
	}
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "device"
	}
	return h
}
