package config

import (
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"

	"github.com/apv022/sol/internal/netutil"
)

// Option is one setting a user can read and change from the CLI
// (`sol config`) or the panel, without editing config.json by hand. It
// never carries a secret.
type Option struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Value   string `json:"value"`
	Kind    string `json:"kind"` // "bool", "text", "port" or "list" (comma-separated)
	Help    string `json:"help"`
	Restart bool   `json:"restart"` // takes effect when the node restarts
}

// optionDef is an Option plus how to read and write it (under c.mu).
type optionDef struct {
	Option
	get func(c *Config) string
	set func(c *Config, v string) error
}

// optionDefs is the list, in display order.
var optionDefs = []optionDef{
	{Option{Key: "name", Label: "Name", Kind: "text", Help: "What your peers see this machine called."},
		func(c *Config) string { return c.Name },
		func(c *Config, v string) error { c.Name = v; return nil }},
	{Option{Key: "tor", Label: "Tor", Kind: "bool", Restart: true,
		Help: "Reach peers and be reached over Tor. Works behind any router and hides your IP address. Leave on unless you only use Sol on one LAN."},
		func(c *Config) string { return onOff(c.Transports.Tor) },
		func(c *Config, v string) error { return setBool(&c.Transports.Tor, v) }},
	{Option{Key: "direct", Label: "Direct mode", Kind: "bool", Restart: true,
		Help: "Connect straight to peers over WireGuard when possible (much faster than Tor), and over Tor otherwise. Your peers then see your IP addresses."},
		func(c *Config) string { return onOff(c.Transports.Direct) },
		func(c *Config, v string) error { return setBool(&c.Transports.Direct, v) }},
	{Option{Key: "direct_listen", Label: "Direct mode port", Kind: "text", Restart: true,
		Help: "Address direct mode listens on, UDP and TCP (default :7701). Allow it in your firewall."},
		func(c *Config) string { return c.Transports.DirectListen },
		func(c *Config, v string) error { c.Transports.DirectListen = v; return nil }},
	{Option{Key: "direct_advertise", Label: "Forwarded addresses", Kind: "list", Restart: true,
		Help: "If you forwarded the direct-mode port on your router, the public IP:port peers should use (comma-separated). Empty: Sol works it out."},
		func(c *Config) string { return strings.Join(c.Transports.DirectAdvertise, ", ") },
		func(c *Config, v string) error {
			list := splitList(v)
			for _, a := range list {
				if err := netutil.DirectCandidate(a, c.directOnLoopback()); err != nil {
					return fmt.Errorf("%q: %v", a, err)
				}
			}
			c.Transports.DirectAdvertise = list
			return nil
		}},
	{Option{Key: "stun", Label: "STUN servers", Kind: "list", Restart: true,
		Help: "Servers to ask for your public address in direct mode: cloudflare, google, mozilla, twilio, or host:port (comma-separated). Empty (the default): none; your paired peers tell you instead. A STUN server learns your public IP."},
		func(c *Config) string { return strings.Join(c.Transports.STUN, ", ") },
		func(c *Config, v string) error { c.Transports.STUN = splitList(v); return nil }},
	{Option{Key: "clearnet", Label: "LAN transport (unencrypted)", Kind: "bool", Restart: true,
		Help: "Plain TCP for one trusted LAN and for testing. Traffic is not encrypted and your LAN IP is shown. Leave off otherwise."},
		func(c *Config) string { return onOff(c.Transports.Clearnet) },
		func(c *Config, v string) error { return setBool(&c.Transports.Clearnet, v) }},
	{Option{Key: "clearnet_listen", Label: "LAN transport address", Kind: "text", Restart: true,
		Help: "Address the LAN transport listens on (default 0.0.0.0:7700)."},
		func(c *Config) string { return c.Transports.ClearnetListen },
		func(c *Config, v string) error {
			if _, _, err := net.SplitHostPort(v); err != nil {
				return fmt.Errorf("want host:port, e.g. 0.0.0.0:7700")
			}
			c.Transports.ClearnetListen = v
			return nil
		}},
	{Option{Key: "panel_port", Label: "Panel port", Kind: "port", Restart: true,
		Help: "Port of this panel on 127.0.0.1 (default 8080)."},
		func(c *Config) string { return strconv.Itoa(c.PanelPort) },
		func(c *Config, v string) error { return setPort(&c.PanelPort, v) }},
	{Option{Key: "node_port", Label: "Node port", Kind: "port", Restart: true,
		Help: "Local port Tor forwards peers to, on 127.0.0.1 (default 8081). Change it only if something else uses it."},
		func(c *Config) string { return strconv.Itoa(c.NodePort) },
		func(c *Config, v string) error { return setPort(&c.NodePort, v) }},
	{Option{Key: "help_tor", Label: "Help the Tor network", Kind: "bool",
		Help: "Run a Snowflake proxy so people in censored countries can reach Tor through this machine. Contacts the Tor Project and public STUN servers from your IP address."},
		func(c *Config) string { return onOff(c.HelpTor) },
		func(c *Config, v string) error { return setBool(&c.HelpTor, v) }},
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func setBool(dst *bool, v string) error {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "on", "true", "yes", "1":
		*dst = true
	case "off", "false", "no", "0":
		*dst = false
	default:
		return fmt.Errorf("want on or off, got %q", v)
	}
	return nil
}

func setPort(dst *int, v string) error {
	p, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("want a port number from 1 to 65535, got %q", v)
	}
	*dst = p
	return nil
}

// splitList splits a comma- or space-separated list, dropping empty items.
func splitList(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' })
}

// Options returns every setting with its current value.
func (c *Config) Options() []Option {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Option, 0, len(optionDefs))
	for _, d := range optionDefs {
		o := d.Option
		o.Value = d.get(c)
		out = append(out, o)
	}
	return out
}

// SetOption changes one setting by key, validates the whole config, and
// saves. On any error nothing changes. It reports whether the node must
// restart for the change to take effect.
func (c *Config) SetOption(key, value string) (restart bool, err error) {
	i := slices.IndexFunc(optionDefs, func(d optionDef) bool { return d.Key == key })
	if i < 0 {
		return false, fmt.Errorf("unknown setting %q; `sol config` lists them", key)
	}
	d := optionDefs[i]
	c.mu.Lock()
	defer c.mu.Unlock()
	name, helpTor, panel, node, tr := c.Name, c.HelpTor, c.PanelPort, c.NodePort, c.cloneTransports()
	rollback := func() { c.Name, c.HelpTor, c.PanelPort, c.NodePort, c.Transports = name, helpTor, panel, node, tr }
	if err := d.set(c, strings.TrimSpace(value)); err != nil {
		rollback()
		return false, fmt.Errorf("%s: %w", key, err)
	}
	if err := c.validateLocked(); err != nil {
		rollback()
		return false, err
	}
	if err := c.saveLocked(); err != nil {
		rollback()
		return false, err
	}
	return d.Restart, nil
}
