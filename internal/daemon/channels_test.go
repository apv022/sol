package daemon

import "testing"

func TestElectHolder(t *testing.T) {
	devs := []DeviceInfo{
		{ID: "c", Online: true},
		{ID: "a", Online: false, Anchor: true}, // an anchor that is down does not hold it
		{ID: "b", Online: true, This: true},
	}
	if h, ok := electHolder(devs); !ok || h.ID != "b" {
		t.Fatalf("lowest online: %+v", h)
	}
	devs[0].Anchor = true
	if h, _ := electHolder(devs); h.ID != "c" {
		t.Fatalf("anchor first: %+v", h)
	}
	devs[1].Online = true
	if h, _ := electHolder(devs); h.ID != "a" {
		t.Fatalf("lowest anchor: %+v", h)
	}
	if _, ok := electHolder([]DeviceInfo{{ID: "x"}}); ok {
		t.Fatal("a holder among devices that are all down")
	}
}

// TestChannelActionsNeedApprovalForAgents: what changes which code runs on this device, or
// whom it trusts for it, waits for the person when an agent asks.
func TestChannelActionsNeedApprovalForAgents(t *testing.T) {
	for _, name := range []string{"sol.apps.follow", "sol.apps.install", "sol.apps.update", "sol.apps.auto", "sol.apps.rollback"} {
		s, ok := solSpec(name)
		if !ok || s.Approval != "agents" {
			t.Errorf("%s: %+v", name, s)
		}
	}
	for _, name := range []string{"sol.apps.channels", "sol.apps.available", "sol.apps.updates"} {
		if s, ok := solSpec(name); !ok || s.Effect != "read" {
			t.Errorf("%s: %+v", name, s)
		}
	}
}
