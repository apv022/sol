package audit

import (
	"context"
	"testing"

	"github.com/apv022/sol/internal/config"
)

func TestRunOnlyAnonymousServices(t *testing.T) {
	cfg := config.Default()
	cfg.Services = []config.Service{
		{Name: "music", Visibility: config.VisibilityPrivate},
		{Name: "blog", Visibility: config.VisibilityAnonymousPublic},
	}
	rs := Run(context.Background(), Env{Config: cfg}, "")
	if len(rs) != len(Checks) {
		t.Fatalf("got %d results, want %d", len(rs), len(Checks))
	}
	for _, r := range rs {
		if r.Service != "blog" {
			t.Fatalf("audited a non-anonymous service: %+v", r)
		}
	}
	if Failed(rs) {
		t.Fatalf("default config should pass: %+v", rs)
	}

	cfg.Transports.Clearnet = true
	if !Failed(Run(context.Background(), Env{Config: cfg}, "blog")) {
		t.Fatal("clearnet enabled should fail the audit")
	}
}
