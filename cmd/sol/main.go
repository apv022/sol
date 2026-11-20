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
//	sol do <app>.<action> ...    run an action (sol do --help); as an agent with SOL_AGENT_KEY or --as-agent
//	sol agent new|list|revoke|renew   agent keys
//	sol inbox [approve|deny <id>]     notices and approvals
//	sol theme list|set|settings       the look of every app
//	sol backup words|confirm          the 24 recovery words
//	sol restore --words "..."         set up a fresh directory from the words
//	sol network-changed [reason]      redial peers now (OS network callbacks)
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

  init [--name N --device D | --join] [--listen ADDR] [--transport NAME] [--ui ADDR] [--files DIR]
  run
  status [--json]
  invite [--join]
  pair <link>
  apps [--json]
  app install <dir> | app remove <id> | app run <id>
  do <app>.<action> [--name value ...] [--args JSON|-] [--json] [--dry-run] [--person ID] [--as-agent NAME]
  agent new <name> --scope PATTERN... [--person ID|self] [--days N] [--out FILE] | agent list | agent revoke <name|id> | agent renew <name> [--days N]
  inbox [--json] | inbox approve <id> | inbox deny <id>
  theme list | theme set <name> | theme settings [--scheme S] [--accent #rrggbb] [--density D] [--radius R] [--glass on|off] [--font F]
  backup words | backup confirm
  restore --words "..." [--from LINK] [--name N] [--device D] [init's --listen/--transport/--ui/--files]
  network-changed [reason]

Every command takes --dir DIR (default $SOL_DIR or ~/.sol).`

func run(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Println(usage)
		return nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "app", "agent", "theme", "backup":
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			return errors.New(usage)
		}
		cmd, rest = cmd+" "+rest[0], rest[1:]
	case "inbox":
		if len(rest) > 0 && (rest[0] == "approve" || rest[0] == "deny") {
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
		tr := fs.String("transport", "direct", "transport: direct, clearnet, wg or tor")
		ui := fs.String("ui", "127.0.0.1:7777", "address of the app UIs")
		files := fs.String("files", "", "the synced folder (default ~/SolDrive)")
		words := fs.String("words", "", "restore: the 24 recovery words")
		from := fs.String("from", "", "restore: a pair or join link of a device that knows you (yours or a contact's)")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		cfg := daemon.Config{UI: *ui, Files: absOrEmpty(*files)}
		if *listen != "" || *tr != "direct" {
			cfg.Transports = []daemon.TransportConfig{{Name: *tr, Listen: *listen}}
		}
		if cmd == "restore" {
			if *words == "" {
				return errors.New("restore: --words is required")
			}
			if err := daemon.Restore(context.Background(), *dir, daemon.RestoreOptions{Words: *words, Name: *name,
				Device: *device, From: *from, Config: cfg, Builtins: builtins.All()}); err != nil {
				return err
			}
			fmt.Printf("Restored in %s. Start it with `sol run`; your other devices and contacts need pairing again.\n", *dir)
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
		fmt.Fprintln(tw, "NAME\tSCOPES\tEXPIRES\tSTATE")
		for _, a := range as {
			state := "active"
			if a.Revoked {
				state = "revoked"
			} else if time.Now().After(a.Expires) {
				state = "expired"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", a.Name, strings.Join(a.Scopes, " "), a.Expires.Local().Format("2006-01-02 15:04"), state)
		}
		return tw.Flush()
	case "agent revoke":
		if fs.NArg() != 1 {
			return errors.New("usage: sol agent revoke <name|id>")
		}
		if err := c.Do(ctx, "POST", "/admin/v1/agents/revoke", map[string]string{"name": fs.Arg(0)}, nil); err != nil {
			return err
		}
		fmt.Println("Revoked", fs.Arg(0), "on all your devices.")
		return nil
	case "agent renew":
		if fs.NArg() != 1 {
			return errors.New("usage: sol agent renew <name> [--days N]")
		}
		var a daemon.AgentInfo
		if err := c.Do(ctx, "POST", "/admin/v1/agents/renew", map[string]any{"name": fs.Arg(0), "days": *days}, &a); err != nil {
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
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", it.ID, it.App, it.Kind, title, state, it.Time.Local().Format("2006-01-02 15:04"))
		}
		return tw.Flush()
	case "inbox approve", "inbox deny":
		if fs.NArg() != 1 {
			return errors.New("usage: sol " + cmd + " <id>")
		}
		var it sol.InboxItem
		if err := c.Do(ctx, "POST", "/admin/v1/inbox/answer", map[string]any{"id": fs.Arg(0), "approve": cmd == "inbox approve"}, &it); err != nil {
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
		var w struct{ Words []string }
		if err := c.Do(ctx, "GET", "/admin/v1/backup/words", nil, &w); err != nil {
			return err
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
	case "network-changed":
		return c.Do(ctx, "POST", "/admin/v1/network-changed", map[string]string{"reason": strings.Join(fs.Args(), " ")}, nil)
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
		person = st.Person
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
