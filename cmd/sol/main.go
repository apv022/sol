// Command sol is the Sol daemon, its CLI, and the first-party apps in one binary.
//
//	sol init [--name N --device D | --join] [--listen ADDR] [--transport NAME]... [--tor-bin PATH] [--ui ADDR] [--files DIR]
//	sol                          start this device in the background (same as sol start); on a
//	                             machine with no data directory it sets Sol up first and opens it
//	sol start | stop | restart   manage the background daemon
//	sol logs [-f] [-n N]         what the background daemon logged
//	sol autostart on|off|status  start Sol when you log in (systemd user unit, launchd agent)
//	sol launcher on|off|status   the "Sol" app-menu entry (Linux .desktop; `autostart` manages it too)
//	sol run                      run this device in the foreground (what the above run)
//	sol status                   who I am, my endpoints and sessions
//	sol invite [--join|--anonymous]  print a one-time pair (or join, or anonymous pair) link
//	sol people [set <name> --anonymous|--buddy]  list contacts; make one Tor-only (anonymous) or a buddy
//	sol transports [enable|disable <name>]  list the transports (wg, direct, tor) and nearby (LAN discovery); turn one on or off
//	sol pair <link>              redeem a pair link, or a join link on a fresh --join device
//	sol apps                     list installed apps and their UI addresses
//	sol app install <dir>        install (or replace) the app in dir
//	sol app remove <id>          remove an app (its data stays; an external app's token is revoked)
//	sol app connect < req.json   register an external app (design/APPS_EVERYWHERE.md M5)
//	sol app run <id>             run a builtin app in the foreground (the daemon does this)
//	sol do <app>.<action> ...    run an action (sol do --help); as an agent with SOL_AGENT_KEY or --as-agent
//	sol agent new|list|revoke|renew   agent keys
//	sol inbox [approve|deny <id>]     notices and approvals
//	sol events [--json]               follow the admin event stream, one event per line
//	sol notifications on|off|status   desktop notifications of Inbox items (this device)
//	sol theme list|set|settings       the look of every app
//	sol backup words|confirm          the 24 recovery words
//	sol restore --words "..."         set up a fresh directory from the words
//	sol network-changed [reason]      redial peers now (OS network callbacks)
//	sol nearby ad|match|interfaces|report|ble-ad|ble-match|ble-report   the Android shell's Wi-Fi Direct and Bluetooth LE plumbing (JSON)
//	sol open [--print] [app]          open the Sol app (or one app, e.g. `sol open files`) in the browser, logged in
//	sol service add|list|remove       share a local port with people (port sharing)
//	sol tunnel open|list|close        open a contact's service on a local port
//
// Every command takes --dir (default $SOL_DIR, else ~/.local/share/sol on Linux, ~/Library/Application
// Support/Sol on macOS, %LOCALAPPDATA%\Sol on Windows). The logic lives in internal/daemon;
// this file only parses arguments and prints.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/apv022/sol/internal/apphost"
	"github.com/apv022/sol/internal/builtins"
	"github.com/apv022/sol/internal/cli"
	"github.com/apv022/sol/internal/daemon"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/sdk/go/sol"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "do" {
		os.Exit(doCmd(os.Args[2:]))
	}
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sol:", err)
		os.Exit(1)
	}
}

// doCmd is `sol do`: it takes --dir and --as-agent out of the arguments, connects as the admin
// or as an agent (--as-agent NAME, or SOL_AGENT_KEY=<key file>), and hands over to cli.Do.
func doCmd(args []string) int {
	dir, agent := daemon.DefaultDir(), ""
	var rest []string
	for i := 0; i < len(args); i++ {
		k, v, has := strings.Cut(args[i], "=")
		if k == "--dir" || k == "--as-agent" {
			if !has {
				if i+1 >= len(args) {
					fmt.Fprintln(os.Stderr, "sol do:", k, "needs a value")
					return cli.ExitUsage
				}
				i++
				v = args[i]
			}
			if k == "--dir" {
				dir = v
			} else {
				agent = v
			}
			continue
		}
		rest = append(rest, args[i])
	}
	key := os.Getenv("SOL_AGENT_KEY")
	if agent != "" {
		key = agentKeyPath(dir, agent)
	}
	var c *daemon.Client
	var err error
	if key != "" {
		c, err = daemon.ConnectAgent(dir, key)
	} else {
		c, err = daemon.Connect(dir)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sol do:", err)
		return cli.ExitError
	}
	return cli.Do(context.Background(), c, rest, os.Stdin, os.Stdout, os.Stderr)
}

// agentKeyPath is where `sol agent new` keeps an agent's key file by default.
func agentKeyPath(dir, name string) string { return filepath.Join(dir, "agents", name+".key") }

// list is a repeatable string flag.
type list []string

// String implements flag.Value.
func (l *list) String() string { return strings.Join(*l, ",") }

// Set implements flag.Value.
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

const usage = `usage: sol <command> [flags]

  init [--name N --device D | --join] [--listen ADDR] [--transport NAME]... [--tor-bin PATH] [--ui ADDR] [--files DIR]
  (no command) | start | stop | restart       run Sol in the background; stop it; restart it
  logs [-f] [-n N]                  what the background daemon logged (-f follows)
  autostart on|off|status           start Sol when you log in (and add Sol to the app menu)
  launcher on|off|status            the "Sol" entry in the app menu (Linux)
  run                               run in the foreground
  status [--json]
  invite [--join | --anonymous]
  people [--json] | people set <name> --anonymous|--buddy
  transports [--json] | transports enable <name> | transports disable <name>
  pair <link>
  apps [--json]
  app install <dir> | app remove <id> | app run <id>
  app connect < request.json        register an external app (JSON {manifest, package, cert} on stdin); prints its token once
  do <app>.<action> [--name value ...] [--args JSON|-] [--json] [--dry-run] [--person ID] [--as-agent NAME]
  agent new <name> --scope PATTERN... [--person ID|self] [--days N] [--out FILE] | agent list | agent revoke <name|id> | agent renew <name> [--days N]
  inbox [--json] | inbox approve <id> | inbox deny <id> | inbox dismiss <id>
  events [--json]                   follow the admin events (inbox items, apps, ...) until interrupted
  notifications on|off|status       desktop notifications for Inbox items on this device (SOL_NOTIFY=off|on overrides)
  theme list | theme set <name> | theme settings [--scheme S] [--accent #rrggbb] [--density D] [--radius R] [--glass on|off] [--font F]
  version                           this binary's version, and the running Sol's when it differs
  backup words | backup confirm
  reset --confirm NAME              remove Sol from this device (identity archived, synced folder kept), then set up again
  restore --words "..." [--from LINK] [--name N] [--device D] [init's --listen/--transport/--ui/--files]
  network-changed [reason]
  nearby ad [-] | nearby match - | nearby interfaces [IPv4...] | nearby report <state> [detail]
  nearby ble-ad - | nearby ble-match - | nearby ble-report <state> [detail]
                                    the Android app's Wi-Fi Direct and Bluetooth LE plumbing (JSON; requests on stdin)
  open [--print] [app]              open the Sol app, or one app (sol open files), in the browser; --print only prints the URL
  service add <name> <port> [--http] [--with PERSON]... | service list | service remove <name>
  tunnel open <person> <service> [--port N] | tunnel list | tunnel close <id>
  channel new <name> | channel publish --key K --app ID --version V --artifact PLATFORM=FILE... | channel import <record> | channel list | channel remove <name>
                                    publish app releases on this node's public onion (sol channel for more)

Every command takes --dir DIR (default $SOL_DIR, else ~/.local/share/sol on Linux,
~/Library/Application Support/Sol on macOS, %LOCALAPPDATA%\Sol on Windows).`

func run(args []string) error {
	if len(args) == 0 {
		return startCmd(daemon.DefaultDir())
	}
	if args[0] == "version" || args[0] == "--version" || args[0] == "-v" {
		return versionCmd(args[1:])
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Println(usage)
		return nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "channel":
		return channelCmd(rest)
	case "transports", "people", "notifications":
		if cmd == "notifications" && (len(rest) == 0 || strings.HasPrefix(rest[0], "-")) {
			rest = append([]string{"status"}, rest...)
		}
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			rest = append([]string{"list"}, rest...)
		}
		cmd, rest = cmd+" "+rest[0], rest[1:]
	case "app", "agent", "theme", "backup", "service", "tunnel", "nearby":
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			return errors.New(usage)
		}
		cmd, rest = cmd+" "+rest[0], rest[1:]
	case "inbox":
		if len(rest) > 0 && (rest[0] == "approve" || rest[0] == "deny" || rest[0] == "dismiss") {
			cmd, rest = cmd+" "+rest[0], rest[1:]
		}
	}
	fs := flag.NewFlagSet("sol "+cmd, flag.ContinueOnError)
	dir := fs.String("dir", daemon.DefaultDir(), "data directory")
	asJSON := fs.Bool("json", false, "print JSON")
	switch cmd {
	case "init", "restore":
		name := fs.String("name", "", "your name (new person; restore: default the name your contacts know)")
		device := fs.String("device", hostname(), "this device's name")
		join := fs.Bool("join", false, "this device will join an existing person (then: sol pair <join link>)")
		listen := fs.String("listen", "", "transport listen address (default: a free port on 0.0.0.0)")
		var trs list
		fs.Var(&trs, "transport", "a transport to turn on: wg, direct, tor or clearnet; repeatable, or comma-separated (default: wg, direct and tor)")
		ui := fs.String("ui", "127.0.0.1:7777", "address of the app UIs")
		files := fs.String("files", "", "the synced folder (default ~/SolDrive)")
		words := fs.String("words", "", "restore: the 24 recovery words")
		from := fs.String("from", "", "restore: a pair or join link of a device that knows you (yours or a contact's)")
		torBin := fs.String("tor-bin", "", "a tor the platform provides, used before any other (no checksum check; SOL_TOR_BIN overrides it)")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		cfg := daemon.Config{UI: *ui, Files: absOrEmpty(*files), TorBin: absOrEmpty(*torBin)}
		tcs, err := transportConfigs(trs, *listen)
		if err != nil {
			return err
		}
		cfg.Transports = tcs
		if cmd == "restore" {
			if *words == "" {
				return errors.New("restore: --words is required")
			}
			if err := daemon.Restore(context.Background(), *dir, daemon.RestoreOptions{Words: *words, Name: *name,
				Device: *device, From: *from, Config: cfg, Builtins: builtins.All()}); err != nil {
				return err
			}
			fmt.Printf("Restored in %s. Start it with `sol start`; your other devices and contacts need pairing again.\n", *dir)
			return nil
		}
		if !*join && *name == "" {
			return errors.New("init: --name is required (or --join for a device joining an existing person)")
		}
		if err := daemon.Init(*dir, daemon.InitOptions{Name: *name, Device: *device, Join: *join, Config: cfg,
			Builtins: builtins.All()}); err != nil {
			return err
		}
		if *join {
			fmt.Printf("Device created in %s. Start it with `sol start`, then `sol pair <join link>` (from `sol invite --join` on your other device).\n", *dir)
		} else {
			fmt.Printf("Sol is set up for %s in %s. Start it with `sol start`.\n", *name, *dir)
		}
		return nil
	case "run":
		if err := fs.Parse(rest); err != nil {
			return err
		}
		return runDaemon(*dir)
	case "start", "stop", "restart":
		if err := fs.Parse(rest); err != nil {
			return err
		}
		switch cmd {
		case "start":
			return startCmd(*dir)
		case "stop":
			return stopCmd(*dir, false)
		}
		return restartCmd(*dir)
	case "logs":
		return logsCmd(*dir, rest)
	case "events":
		if err := fs.Parse(rest); err != nil {
			return err
		}
		return eventsCmd(*dir, *asJSON)
	case "launcher":
		if err := parseAnywhere(fs, rest); err != nil {
			return err
		}
		return launcherCmd(*dir, fs.Args())
	case "autostart":
		if err := parseAnywhere(fs, rest); err != nil {
			return err
		}
		return autostartCmd(*dir, fs.Args(), *asJSON)
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
	inviteAnon := fs.Bool("anonymous", false, "invite: an anonymous link (only the onion; they become an anonymous contact); people set: Tor only, both ways")
	buddy := fs.Bool("buddy", false, "people set: every transport (the default for a contact)")
	noBrowser := fs.Bool("print", false, "open: only print the login URL")
	confirmName := fs.String("confirm", "", "reset: your name, to confirm removing Sol from this device")
	svcHTTP := fs.Bool("http", false, "service add: it is a web service (contacts can open it in the browser)")
	var people list
	fs.Var(&people, "with", "service add: a person (ID or petname) to share with; repeatable")
	port := fs.Int("port", 0, "tunnel open: the local port (default: the first free from 9000)")
	var scopes list
	fs.Var(&scopes, "scope", "agent new: an action pattern the agent may run (files.*, notes.read, *); repeatable")
	person := fs.String("person", "", "agent new: limit the agent to this person's apps (an ID, or self)")
	days := fs.Int("days", 0, "agent new/renew: lifetime in days (default 30, at most 90)")
	out := fs.String("out", "", "agent new: where to write the key file (default <dir>/agents/<name>.key)")
	settings := map[string]*string{}
	for _, k := range []string{"scheme", "accent", "density", "radius", "glass", "font"} {
		settings[k] = fs.String(k, "\x00", "theme settings: "+k+" (\"\" resets it)")
	}
	if err := parseAnywhere(fs, rest); err != nil {
		return err
	}
	if cmd == "open" && !daemon.Initialised(*dir) {
		return firstRun(*dir) // not set up yet: open (or start) the setup page
	}
	if cmd == "open" {
		if err := ensureRunning(*dir); err != nil {
			return err
		}
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
		fmt.Printf("version  %s\n", st.Version)
		fmt.Printf("person   %s (%s)\ndevice   %s (%s)\nui       %s\nfiles    %s\n", st.Me.Person.Name, st.Me.Person.ID, st.Me.Device.Name, st.Me.Device.ID, st.UI, st.Files)
		for _, e := range st.Endpoints {
			fmt.Printf("endpoint %s %s\n", e.Kind, e.Addr)
		}
		for _, t := range st.Transports {
			fmt.Printf("transport %s %s\n", t.Name, t.State)
		}
		for _, s := range st.PeerSessions {
			fmt.Printf("session  %s %s (%s)\n", s.Relation, s.Device, s.Peer)
		}
		if st.Storage != nil {
			fmt.Printf("storage  %.1f of %.1f GB used\n", float64(st.Storage.Used)/1e9, float64(st.Storage.Total)/1e9)
		}
		return nil
	case "invite":
		var out struct{ Link string }
		if err := c.Do(ctx, "POST", "/admin/v1/invite", map[string]bool{"join": *inviteJoin, "anonymous": *inviteAnon}, &out); err != nil {
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
			running := fmt.Sprint(a.Running)
			if a.External {
				running = "external" // Sol does not run it: it connected from outside (M5)
				if a.Package != "" {
					running += " (" + a.Package + ")"
				}
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", a.ID, a.Version, running, a.URL)
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
	case "app connect":
		// The Android shell runs this after the person approved a connecting app (M5): the
		// request comes on stdin, never argv (other apps can read process arguments), and the
		// answer, which holds the app's token, goes to stdout only.
		if fs.NArg() != 0 {
			return errors.New("usage: sol app connect < {\"manifest\": {...}, \"package\": P, \"cert\": SHA256}")
		}
		var in json.RawMessage
		if err := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&in); err != nil {
			return errors.New("app connect: expected a JSON request on stdin")
		}
		var out daemon.ExternalApp
		if err := c.Do(ctx, "POST", "/admin/v1/apps/external", in, &out); err != nil {
			return err
		}
		return printJSON(out)
	case "app remove":
		if fs.NArg() != 1 {
			return errors.New("usage: sol app remove <id>")
		}
		if err := c.Do(ctx, "POST", "/admin/v1/apps/"+url.PathEscape(fs.Arg(0))+"/remove", nil, nil); err != nil {
			return err
		}
		fmt.Println("Removed", fs.Arg(0))
		return nil
	case "agent new":
		return agentNew(ctx, c, *dir, fs.Args(), scopes, *person, *days, *out)
	case "agent list":
		var as []daemon.AgentInfo
		if err := c.Do(ctx, "GET", "/admin/v1/agents", nil, &as); err != nil {
			return err
		}
		if *asJSON {
			return printJSON(as)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tDEVICE\tSCOPES\tEXPIRES\tSTATE")
		for _, a := range as {
			state := "active"
			if a.Revoked {
				state = "revoked"
			} else if time.Now().After(a.Expires) {
				state = "expired"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", a.Name, a.DeviceName, strings.Join(a.Scopes, " "), a.Expires.Local().Format("2006-01-02 15:04"), state)
		}
		return tw.Flush()
	case "agent revoke":
		if fs.NArg() != 1 {
			return errors.New("usage: sol agent revoke <name|id>")
		}
		if err := c.Do(ctx, "POST", "/admin/v1/agents/"+url.PathEscape(fs.Arg(0))+"/revoke", nil, nil); err != nil {
			return err
		}
		fmt.Println("Revoked", fs.Arg(0), "on all your devices.")
		return nil
	case "agent renew":
		if fs.NArg() != 1 {
			return errors.New("usage: sol agent renew <name> [--days N]")
		}
		var a daemon.AgentInfo
		if err := c.Do(ctx, "POST", "/admin/v1/agents/"+url.PathEscape(fs.Arg(0))+"/renew", map[string]any{"days": *days}, &a); err != nil {
			return err
		}
		p := agentKeyPath(*dir, a.Name)
		if k, err := daemon.LoadAgentKey(p); err == nil {
			k.Cert = a.Cert
			if err := k.Save(p); err != nil {
				return err
			}
		} else {
			fmt.Fprintln(os.Stderr, "note: the key file is not at", p, "; the agent picks the renewal up on its next call")
		}
		fmt.Printf("Renewed %s until %s\n", a.Name, a.Expires.Local().Format("2006-01-02 15:04"))
		return nil
	case "inbox":
		var items []sol.InboxItem
		if err := c.Do(ctx, "GET", "/admin/v1/inbox", nil, &items); err != nil {
			return err
		}
		if *asJSON {
			return printJSON(items)
		}
		if len(items) == 0 {
			fmt.Println("Inbox is empty.")
			return nil
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tAPP\tKIND\tTITLE\tSTATE\tWHEN")
		for _, it := range items {
			state := it.Answer
			if it.Kind == "approval" && state == "" {
				state = "waiting"
			}
			title := it.Title
			if it.Agent != "" {
				title += " (agent " + it.Agent[:8] + ")"
			}
			// An open approval shows the call to approve, secret values already redacted by
			// the daemon; an answered one keeps no args.
			if it.Kind == "approval" && it.Answer == "" && it.Action != nil {
				title += ": " + cli.CallSummary(it.Action.Action, it.Action.Args, 80)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", it.ID, it.App, it.Kind, title, state, it.Time.Local().Format("2006-01-02 15:04"))
		}
		return tw.Flush()
	case "inbox dismiss":
		if fs.NArg() != 1 {
			return errors.New("usage: sol inbox dismiss <id>")
		}
		if err := c.Do(ctx, "POST", "/admin/v1/inbox/"+url.PathEscape(fs.Arg(0))+"/dismiss", nil, nil); err != nil {
			return err
		}
		fmt.Println("Dismissed", fs.Arg(0))
		return nil
	case "inbox approve", "inbox deny":
		if fs.NArg() != 1 {
			return errors.New("usage: sol " + cmd + " <id>")
		}
		var it sol.InboxItem
		verb := strings.TrimPrefix(cmd, "inbox ")
		if err := c.Do(ctx, "POST", "/admin/v1/inbox/"+url.PathEscape(fs.Arg(0))+"/"+verb, nil, &it); err != nil {
			return err
		}
		if *asJSON {
			return printJSON(it)
		}
		fmt.Printf("%s: %s\n", it.ID, it.Answer)
		if it.Error != nil {
			return fmt.Errorf("the action failed: %v", it.Error)
		}
		if len(it.Result) > 0 {
			return cli.Render(os.Stdout, it.Result)
		}
		if it.Outcome == "ok" && it.Agent != "" {
			// An output_secret action's output goes only to the agent that asked (API.md §8).
			fmt.Println("The output goes to the agent that asked.")
		}
		return nil
	case "theme list":
		var st daemon.ThemeState
		if err := c.Do(ctx, "GET", "/admin/v1/themes", nil, &st); err != nil {
			return err
		}
		if *asJSON {
			return printJSON(st)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "\tNAME\tTITLE\tAUTHOR\tSUPPORTS")
		for _, t := range st.Themes {
			mark := ""
			if t.Name == st.Active {
				mark = "*"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", mark, t.Name, t.Title, t.Author, strings.Join(t.Supports, " "))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		for k, v := range st.Settings {
			fmt.Printf("%s: %s\n", k, v)
		}
		return nil
	case "theme set":
		if fs.NArg() != 1 {
			return errors.New("usage: sol theme set <name>")
		}
		if err := c.Do(ctx, "POST", "/admin/v1/theme", map[string]string{"name": fs.Arg(0)}, nil); err != nil {
			return err
		}
		fmt.Println("Theme:", fs.Arg(0))
		return nil
	case "theme settings":
		body := map[string]string{}
		for k, v := range settings {
			if *v != "\x00" {
				body[k] = *v
			}
		}
		var cur map[string]string
		if len(body) == 0 {
			var st daemon.ThemeState
			if err := c.Do(ctx, "GET", "/admin/v1/themes", nil, &st); err != nil {
				return err
			}
			cur = st.Settings
		} else if err := c.Do(ctx, "POST", "/admin/v1/theme/settings", body, &cur); err != nil {
			return err
		}
		return printJSON(cur)
	case "backup words":
		var w daemon.BackupState
		if err := c.Do(ctx, "GET", "/admin/v1/backup", nil, &w); err != nil {
			return err
		}
		if len(w.Words) == 0 {
			return errors.New("no unconfirmed words here: the backup was confirmed, or this device did not create the identity")
		}
		fmt.Println("Write these 24 words on paper, in order, and keep them safe. They are your identity.")
		for i, word := range w.Words {
			fmt.Printf("%2d. %s\n", i+1, word)
		}
		fmt.Println("Then run `sol backup confirm`: this device forgets them and only the paper remains.")
		return nil
	case "backup confirm":
		if err := c.Do(ctx, "POST", "/admin/v1/backup/confirm", nil, nil); err != nil {
			return err
		}
		fmt.Println("Confirmed. The words are gone from this device; keep the paper safe.")
		return nil
	case "reset":
		var r struct {
			Setup string `json:"setup"`
		}
		if err := c.Do(ctx, "POST", "/admin/v1/reset", map[string]string{"confirm": *confirmName}, &r); err != nil {
			return err
		}
		fmt.Println("Removing Sol from this device. Your identity is moved aside into", filepath.Join(*dir, "removed-<time>"), "(move it back to undo); the synced folder is untouched.")
		fmt.Println("Set Sol up again with `sol open` (start new, add this device to your Sol, or restore from your backup words).")
		return nil
	case "network-changed":
		return c.Do(ctx, "POST", "/admin/v1/network-changed", map[string]string{"reason": strings.Join(fs.Args(), " ")}, nil)
	case "notifications on", "notifications off", "notifications status":
		return notificationsCmd(ctx, c, cmd, *asJSON)
	case "open":
		return openUI(ctx, c, *noBrowser, fs.Args())
	case "service add", "service list", "service remove", "tunnel open", "tunnel list", "tunnel close":
		return portCmd(ctx, c, cmd, fs.Args(), *asJSON, *svcHTTP, people, *port)
	case "transports list", "transports enable", "transports disable":
		return transportsCmd(ctx, c, cmd, fs.Args(), *asJSON)
	case "nearby ad", "nearby match", "nearby interfaces", "nearby report", "nearby ble-ad", "nearby ble-match", "nearby ble-report":
		return nearbyCmd(ctx, c, cmd, fs.Args(), os.Stdin, os.Stdout)
	case "people list", "people set":
		return peopleCmd(ctx, c, cmd, fs.Args(), *asJSON, *inviteAnon, *buddy)
	}
	return fmt.Errorf("unknown command %q\n%s", cmd, usage)
}

// runDaemon is `sol run`: start, print where things are, wait for a signal, stop.
func runDaemon(dir string) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	token := "" // the setup token a "Remove Sol from this device" handed the browser
	for {
		join := ""
		if !daemon.Initialised(dir) {
			// No identity: serve setup in the browser until the person picks (daemon/setup.go).
			res, err := daemon.RunSetup(ctx, daemon.SetupOptions{Dir: dir, Builtins: builtins.All(), Token: token,
				Name: accountName(), Device: hostname(), Log: log})
			if ctx.Err() != nil || errors.Is(err, daemon.ErrSetupStopped) {
				return nil
			}
			if err != nil {
				return err
			}
			join = res.JoinLink
		}
		d, err := daemon.Start(daemon.Options{Dir: dir, Builtins: builtins.All(), Log: log,
			OnApps: func(apps []apphost.MenuApp) { syncLauncher(dir, apps) }})
		if errors.Is(err, daemon.ErrRunning) && os.Getenv("SOL_SUPERVISED") == "1" {
			return nil // exit 0: the login entry must not restart a duplicate forever
		}
		if err != nil {
			return err
		}
		if join != "" {
			go func() {
				if _, _, err := d.Pair(ctx, join); err != nil {
					log.Warn("joining with the link from setup failed; paste a new one in Devices or run `sol pair <link>`", "err", err)
				}
			}()
		}
		st := d.Status()
		var eps []string
		for _, e := range st.Endpoints {
			eps = append(eps, e.Kind+" "+e.Addr)
		}
		fmt.Printf("Sol is running for %s on %s.\n  open it:   sol open\n  endpoints: %s\nCtrl-C (or `sol stop`) to stop.\n", st.Me.Person.Name, st.Me.Device.Name, strings.Join(eps, ", "))
		select {
		case <-ctx.Done():
		case <-d.ShutdownRequested():
		}
		tok, reset := d.ResetRequested()
		if err := d.Close(); err != nil || !reset {
			return err
		}
		// "Remove Sol from this device": archive the identity, then serve setup on the same port.
		archived, err := daemon.ArchiveIdentity(dir)
		if err != nil {
			return fmt.Errorf("removing Sol from this device: %w", err)
		}
		log.Info("removed Sol from this device; the old identity is archived (move it back to undo)", "path", archived)
		token = tok
	}
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

// agentNew is `sol agent new`: the key pair is made here, so only the public half goes to the
// daemon, which signs the certificate; the key file is written 0600 where the agent runs.
func agentNew(ctx context.Context, c *daemon.Client, dir string, args, scopes []string, person string, days int, out string) error {
	if len(args) != 1 {
		return errors.New("usage: sol agent new <name> --scope PATTERN... [--person ID|self] [--days N] [--out FILE]")
	}
	name := args[0]
	if person == "self" {
		var st daemon.Status
		if err := c.Do(ctx, "GET", "/admin/v1/status", nil, &st); err != nil {
			return err
		}
		person = st.Me.Person.ID
	}
	if person != "" {
		scopes = append(scopes, "person:"+person)
	}
	k, pub, err := daemon.NewAgentKey(name)
	if err != nil {
		return err
	}
	var a daemon.AgentInfo
	if err := c.Do(ctx, "POST", "/admin/v1/agents", map[string]any{"name": name, "key": identity.ID(pub), "scopes": scopes, "days": days}, &a); err != nil {
		return err
	}
	k.Cert = a.Cert
	if out == "" {
		out = agentKeyPath(dir, name)
	}
	out = absOrEmpty(out)
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return err
	}
	if err := k.Save(out); err != nil {
		return err
	}
	fmt.Printf("Agent %s may run %s until %s.\nKey file: %s\nUse it with:\n  export SOL_AGENT_KEY=%s\nor: sol do --as-agent %s ...\n",
		name, strings.Join(scopes, " "), a.Expires.Local().Format("2006-01-02"), out, out, name)
	return nil
}

// parseAnywhere parses flags that may come before or after the positional arguments
// (`sol agent new bot --scope x`); fs.Args() then holds the positionals in order.
func parseAnywhere(fs *flag.FlagSet, args []string) error {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	return fs.Parse(append([]string{"--"}, pos...))
}

// openApp is `sol open <app>`: the app's own origin in the browser. A browser of this OS user
// is signed in there by the daemon (design/ADMIN.md "Who may use it"); any other is sent
// through the Sol app's sign-in, which comes back to the app.
func openApp(ctx context.Context, c *daemon.Client, id string, printOnly bool) error {
	var apps []daemon.AppInfo
	if err := c.Do(ctx, "GET", "/admin/v1/apps", nil, &apps); err != nil {
		return err
	}
	var have []string
	for _, a := range apps {
		if a.URL == "" {
			continue
		}
		if strings.EqualFold(a.ID, id) || strings.EqualFold(a.Name, id) {
			fmt.Println(a.URL)
			if !printOnly && !openBrowser(a.URL) {
				fmt.Fprintln(os.Stderr, "Could not start a browser; open the link above.")
			}
			return nil
		}
		have = append(have, a.ID)
	}
	if len(have) == 0 {
		return fmt.Errorf("no app %q: no installed app has a UI", id)
	}
	return fmt.Errorf("no app %q with a UI; installed: %s", id, strings.Join(have, ", "))
}

// openUI is `sol open [app]`: with an app, that app's origin (openApp); without, a one-time
// login code and the browser at /login?code=.
func openUI(ctx context.Context, c *daemon.Client, printOnly bool, args []string) error {
	if len(args) > 1 {
		return errors.New("usage: sol open [--print] [app]")
	}
	if len(args) == 1 {
		return openApp(ctx, c, args[0], printOnly)
	}
	var lc struct {
		URL string `json:"url"`
	}
	if err := c.Do(ctx, "POST", "/admin/v1/login-code", nil, &lc); err != nil {
		return err
	}
	fmt.Println(lc.URL)
	if printOnly {
		return nil
	}
	if !openBrowser(lc.URL) {
		fmt.Fprintln(os.Stderr, "Could not start a browser; open the link above within a minute.")
	}
	return nil
}

// openBrowser starts the default browser on url; false if it could not. SOL_NO_BROWSER=1
// (tests, scripts) never opens one: xdg-open reaches the desktop session's browser even when
// DISPLAY points elsewhere.
func openBrowser(url string) bool {
	if os.Getenv("SOL_NO_BROWSER") == "1" {
		return false
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return false
	}
	go func() { _ = cmd.Wait() }()
	return true
}

// portCmd is `sol service ...` and `sol tunnel ...` (port sharing).
func portCmd(ctx context.Context, c *daemon.Client, cmd string, args []string, asJSON, httpSvc bool, people []string, port int) error {
	switch cmd {
	case "service add":
		if len(args) != 2 {
			return errors.New("usage: sol service add <name> <port> [--http] [--with PERSON]...")
		}
		p, err := strconv.Atoi(args[1])
		if err != nil {
			return errors.New("the port must be a number")
		}
		ids, err := resolvePeople(ctx, c, people)
		if err != nil {
			return err
		}
		var s daemon.Service
		if err := c.Do(ctx, "POST", "/admin/v1/services", daemon.Service{Name: args[0], Port: p, HTTP: httpSvc, People: ids}, &s); err != nil {
			return err
		}
		fmt.Printf("Sharing 127.0.0.1:%d as %s with %d people.\n", s.Port, s.Name, len(s.People))
		return nil
	case "service list":
		var ss []daemon.Service
		if err := c.Do(ctx, "GET", "/admin/v1/services", nil, &ss); err != nil {
			return err
		}
		if asJSON {
			return printJSON(ss)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tPORT\tHTTP\tPEOPLE")
		for _, s := range ss {
			fmt.Fprintf(tw, "%s\t%d\t%v\t%s\n", s.Name, s.Port, s.HTTP, strings.Join(s.People, " "))
		}
		return tw.Flush()
	case "service remove":
		if len(args) != 1 {
			return errors.New("usage: sol service remove <name>")
		}
		if err := c.Do(ctx, "DELETE", "/admin/v1/services/"+url.PathEscape(args[0]), nil, nil); err != nil {
			return err
		}
		fmt.Println("Stopped sharing", args[0])
		return nil
	case "tunnel open":
		if len(args) != 2 {
			return errors.New("usage: sol tunnel open <person> <service> [--port N]")
		}
		var t daemon.TunnelInfo
		if err := c.Do(ctx, "POST", "/admin/v1/tunnels", map[string]any{"person": args[0], "service": args[1], "port": port}, &t); err != nil {
			return err
		}
		if asJSON {
			return printJSON(t)
		}
		fmt.Printf("%s's %s is on %s (tunnel %s)\n", t.Name, t.Service, t.Addr, t.ID)
		if t.URL != "" {
			fmt.Println(t.URL)
		}
		return nil
	case "tunnel list":
		var ts []daemon.TunnelInfo
		if err := c.Do(ctx, "GET", "/admin/v1/tunnels", nil, &ts); err != nil {
			return err
		}
		if asJSON {
			return printJSON(ts)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tPERSON\tSERVICE\tADDRESS\tSTATE\tACTIVE\tERROR")
		for _, t := range ts {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n", t.ID, t.Name, t.Service, t.Addr, t.State, t.Active, t.LastError)
		}
		return tw.Flush()
	case "tunnel close":
		if len(args) != 1 {
			return errors.New("usage: sol tunnel close <id>")
		}
		if err := c.Do(ctx, "DELETE", "/admin/v1/tunnels/"+url.PathEscape(args[0]), nil, nil); err != nil {
			return err
		}
		fmt.Println("Closed", args[0])
		return nil
	}
	return errors.New(usage)
}

// resolvePeople turns petnames into person IDs (IDs pass through).
func resolvePeople(ctx context.Context, c *daemon.Client, in []string) ([]string, error) {
	out := []string{}
	if len(in) == 0 {
		return out, nil
	}
	var ps []daemon.PersonInfo
	if err := c.Do(ctx, "GET", "/admin/v1/people", nil, &ps); err != nil {
		return nil, err
	}
	for _, x := range in {
		id := ""
		for _, p := range ps {
			if p.ID == x || p.Petname == strings.ToLower(x) {
				id = p.ID
			}
		}
		if id == "" {
			return nil, fmt.Errorf("no contact %q (see sol status / the People page)", x)
		}
		out = append(out, id)
	}
	return out, nil
}

// versionCmd is `sol version`: this binary's version and, when a daemon runs on the data
// directory with another build, that one too (an upgrade that still needs `sol restart`).
func versionCmd(rest []string) error {
	fs := flag.NewFlagSet("sol version", flag.ContinueOnError)
	dir := fs.String("dir", daemon.DefaultDir(), "data directory")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	fmt.Println("sol", daemon.Version)
	if st, err := daemon.Running(*dir); err == nil && st.Version != daemon.Version {
		v := st.Version
		if v == "" {
			v = "an older build"
		}
		fmt.Printf("the running Sol is %s: run `sol restart` to use this one\n", v)
	}
	return nil
}
