package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// Each rule that keeps a setting change from leaving the node unusable,
// with the words the user is shown.
func TestSetOptionRefusesUnusableSettings(t *testing.T) {
	type step struct{ key, value string }
	for _, tc := range []struct {
		name  string
		setup []step // applied first, must succeed
		key   string
		value string
		want  string // substring of the error; "" means it must be allowed
	}{
		{name: "last transport (tor)", key: "tor", value: "off", want: "at least one of Tor"},
		{name: "tor off with direct on", setup: []step{{"direct", "on"}}, key: "tor", value: "off"},
		{name: "tor off with LAN on", setup: []step{{"clearnet", "on"}}, key: "tor", value: "off"},
		{name: "last transport (direct)", setup: []step{{"direct", "on"}, {"tor", "off"}}, key: "direct", value: "off", want: "at least one of Tor"},
		{name: "last transport (LAN)", setup: []step{{"clearnet", "on"}, {"tor", "off"}}, key: "clearnet", value: "off", want: "at least one of Tor"},
		{name: "port zero", key: "panel_port", value: "0", want: "1 to 65535"},
		{name: "port too big", key: "node_port", value: "70000", want: "1 to 65535"},
		{name: "port not a number", key: "panel_port", value: "web", want: "1 to 65535"},
		{name: "panel equals node", key: "panel_port", value: "8081", want: "must differ"},
		{name: "node equals direct", key: "node_port", value: "7701", want: "used by both"},
		{name: "direct equals panel", key: "direct_listen", value: ":8080", want: "used by both"},
		{name: "LAN equals direct", key: "clearnet_listen", value: "0.0.0.0:7701", want: "used by both"},
		{name: "direct port out of range", key: "direct_listen", value: ":99999", want: "bad port"},
		{name: "direct port zero", key: "direct_listen", value: ":0", want: "port from 1 to 65535"},
		{name: "direct address empty", key: "direct_listen", value: "", want: "not a valid host:port"},
		{name: "direct address no port", key: "direct_listen", value: "myhost", want: "want host:port"},
		{name: "LAN port out of range", key: "clearnet_listen", value: "0.0.0.0:0", want: "port from 1 to 65535"},
		{name: "free port", key: "panel_port", value: "9090"},
		{name: "restart-only change", key: "direct", value: "on"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(filepath.Join(t.TempDir(), "config.json"))
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range tc.setup {
				if _, err := c.SetOption(s.key, s.value); err != nil {
					t.Fatalf("setup %v: %v", s, err)
				}
			}
			before := c.Snapshot()
			checkErr := c.CheckOption(tc.key, tc.value)
			_, err = c.SetOption(tc.key, tc.value)
			for _, e := range []error{checkErr, err} {
				if tc.want == "" && e != nil {
					t.Fatalf("refused: %v", e)
				}
				if tc.want != "" && (e == nil || !strings.Contains(e.Error(), tc.want)) {
					t.Fatalf("error %v, want it to contain %q", e, tc.want)
				}
			}
			if tc.want != "" {
				after := c.Snapshot()
				if after.PanelPort != before.PanelPort || after.NodePort != before.NodePort || after.Transports.Tor != before.Transports.Tor ||
					after.Transports.DirectListen != before.Transports.DirectListen || after.Transports.Direct != before.Transports.Direct {
					t.Fatal("a refused change was applied")
				}
			}
		})
	}
}

// CheckOption never changes anything, and a service or tunnel port that
// Sol would also bind is a collision too.
func TestCheckOptionIsReadOnlyAndSeesServicePorts(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AddService(Service{Name: "web", LocalPort: 9000, Visibility: VisibilityPrivate}); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckOption("panel_port", "9001"); err != nil || c.Snapshot().PanelPort != 8080 {
		t.Fatalf("CheckOption: %v, panel %d", err, c.Snapshot().PanelPort)
	}
	if err := c.CheckOption("panel_port", "9000"); err == nil || !strings.Contains(err.Error(), `service "web"`) {
		t.Fatalf("service collision: %v", err)
	}
}

// A config that already breaks a rule (hand-edited) can still be repaired.
func TestSetOptionAllowsRepairingAnExistingProblem(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.Transports.Tor = false
	c.mu.Unlock()
	if _, err := c.SetOption("help_tor", "on"); err != nil {
		t.Fatalf("unrelated change on a config with all transports off: %v", err)
	}
	if _, err := c.SetOption("tor", "on"); err != nil {
		t.Fatalf("repair: %v", err)
	}
}
