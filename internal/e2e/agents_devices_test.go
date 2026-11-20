package e2e

import (
	"testing"

	sold "github.com/apv022/sol/internal/daemon"
)

// TestAgentsAcrossDevices: an agent issued on the laptop is listed on the phone with the
// issuing device named, and revoking it from the phone (by name) reaches the laptop through
// the replicated revocations feed.
func TestAgentsAcrossDevices(t *testing.T) {
	laptop := startNode(t, sold.InitOptions{Name: "Alice", Device: "laptop"}, nil)
	phone := joinNode(t, laptop, nil)
	waitSession(t, laptop, phone)
	waitSession(t, phone, laptop)
	newAgent(t, laptop, "bot", "files.*")
	find := func(d *sold.Daemon) (sold.AgentInfo, bool) {
		for _, a := range d.AllAgents() {
			if a.Name == "bot" {
				return a, true
			}
		}
		return sold.AgentInfo{}, false
	}
	eventually(t, "the phone lists the laptop's agent", func() bool { _, ok := find(phone); return ok })
	a, _ := find(phone)
	if a.DeviceName != "laptop" || a.Revoked || len(a.Cert) != 0 {
		t.Fatalf("%+v", a)
	}
	if l, _ := find(laptop); l.DeviceName != "laptop" {
		t.Fatalf("local: %+v", l)
	}
	if _, err := phone.RevokeAgent("bot"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the laptop sees it revoked", func() bool { l, _ := find(laptop); return l.Revoked })
	if p, _ := find(phone); !p.Revoked {
		t.Fatalf("phone: %+v", p)
	}
}
