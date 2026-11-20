package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/apv022/sol/internal/daemon"
)

// transportConfigs turns init's --transport flags (repeatable, or comma-separated) and
// --listen into the config's transports; none means the defaults (wg, direct and tor).
// --listen applies to every listening transport given (WireGuard takes the UDP port, TLS
// direct the TCP port of the same number).
func transportConfigs(names []string, listen string) ([]daemon.TransportConfig, error) {
	var want []string
	for _, n := range names {
		for _, p := range strings.Split(n, ",") {
			if p = strings.TrimSpace(p); p != "" && !slices.Contains(want, p) {
				want = append(want, p)
			}
		}
	}
	if len(want) == 0 {
		if listen == "" {
			return nil, nil // daemon.Init picks the defaults
		}
		want = daemon.KnownTransports
	}
	var out []daemon.TransportConfig
	for _, n := range want {
		switch n {
		case "wg", "direct", "clearnet":
			out = append(out, daemon.TransportConfig{Name: n, Listen: listen})
		case "tor":
			out = append(out, daemon.TransportConfig{Name: n})
		default:
			return nil, fmt.Errorf("unknown transport %q (wg, direct, tor or clearnet)", n)
		}
	}
	if listen == "" {
		def := map[string]daemon.TransportConfig{}
		for _, tc := range daemon.DefaultTransports() {
			def[tc.Name] = tc
		}
		for i, tc := range out {
			if tc.Listen == "" && tc.Name != "tor" {
				out[i].Listen = def[tc.Name].Listen
				if out[i].Listen == "" {
					out[i].Listen = def["direct"].Listen
				}
			}
		}
	}
	return out, nil
}

// transportsCmd is `sol transports [list] | enable <name> | disable <name>`.
func transportsCmd(ctx context.Context, c *daemon.Client, cmd string, args []string, asJSON bool) error {
	var ts []daemon.TransportState
	switch cmd {
	case "transports list":
		if err := c.Do(ctx, "GET", "/admin/v1/transports", nil, &ts); err != nil {
			return err
		}
	default:
		if len(args) != 1 {
			return errors.New("usage: sol " + cmd + " <wg|direct|tor>")
		}
		on := cmd == "transports enable"
		if err := c.Do(ctx, "POST", "/admin/v1/transports/"+args[0], map[string]bool{"enabled": on}, &ts); err != nil {
			return err
		}
	}
	if asJSON {
		return printJSON(ts)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tON\tSTATE\tDETAIL")
	for _, t := range ts {
		on := "no"
		if t.Enabled {
			on = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", t.Name, on, t.State, t.Detail)
	}
	return tw.Flush()
}

// peopleCmd is `sol people [list]` and `sol people set <name> --anonymous|--buddy`.
func peopleCmd(ctx context.Context, c *daemon.Client, cmd string, args []string, asJSON, anonymous, buddy bool) error {
	if cmd == "people list" {
		var ps []daemon.PersonInfo
		if err := c.Do(ctx, "GET", "/admin/v1/people", nil, &ps); err != nil {
			return err
		}
		if asJSON {
			return printJSON(ps)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tPETNAME\tPRIVACY\tONLINE\tID")
		for _, p := range ps {
			priv := "buddy"
			if p.Anonymous {
				priv = "anonymous"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%v\t%s\n", p.Name, p.Petname, priv, p.Online, p.ID)
		}
		return tw.Flush()
	}
	if len(args) != 1 || anonymous == buddy {
		return errors.New("usage: sol people set <name|id> --anonymous|--buddy")
	}
	id, err := personID(ctx, c, args[0])
	if err != nil {
		return err
	}
	var p daemon.PersonInfo
	if err := c.Do(ctx, "PATCH", "/admin/v1/people/"+id, map[string]bool{"anonymous": anonymous}, &p); err != nil {
		return err
	}
	if asJSON {
		return printJSON(p)
	}
	if anonymous {
		fmt.Printf("%s is anonymous now. %s\n", p.Name, daemon.AnonymousWarning)
	} else {
		fmt.Printf("%s is a buddy now: Sol uses WireGuard and TLS direct first, Tor last.\n", p.Name)
	}
	return nil
}

// personID finds a contact by ID, petname or name.
func personID(ctx context.Context, c *daemon.Client, s string) (string, error) {
	var ps []daemon.PersonInfo
	if err := c.Do(ctx, "GET", "/admin/v1/people", nil, &ps); err != nil {
		return "", err
	}
	var found []string
	for _, p := range ps {
		if p.ID == s || strings.EqualFold(p.Petname, s) {
			return p.ID, nil
		}
		if strings.EqualFold(p.Name, s) {
			found = append(found, p.ID)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", fmt.Errorf("no contact called %q (sol people lists them)", s)
	}
	return "", fmt.Errorf("%d contacts are called %q; use the ID (sol people)", len(found), s)
}
