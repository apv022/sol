// Package audit implements `sol audit`: leak checks for services in
// Anonymous Public Mode (docs/spec.md 7.6). The panel shows the same results
// as a "Leak check" card.
//
// Each check is a Check registered in Checks. To add one, implement Check
// and append it to Checks; nothing else changes. Checks that are planned but
// not built yet return StatusSkip with a short explanation, so the output is
// honest about what was and was not verified.
package audit

import (
	"context"
	"fmt"

	"github.com/apv022/sol/internal/config"
)

// Status of one check.
type Status string

const (
	StatusPass Status = "pass"
	StatusFail Status = "fail"
	StatusSkip Status = "skip" // not applicable or not implemented yet
)

// Result is one check outcome.
type Result struct {
	Check   string `json:"check"`
	Service string `json:"service,omitempty"`
	Status  Status `json:"status"`
	Detail  string `json:"detail"` // what was checked and found; never secrets
}

// Env is what checks may inspect. It is read-only.
type Env struct {
	Config *config.Config // a Snapshot, safe to read
}

// Check is one leak check, run once per anonymous-public service.
type Check interface {
	Name() string
	Run(ctx context.Context, env Env, svc config.Service) Result
}

// Checks is the registry, run in order.
var Checks = []Check{
	noClearnet{},
	planned{name: "egress-locked", why: "needs the Docker egress-lock runtime (roadmap v0.5)"},
	planned{name: "headers-stripped", why: "needs an HTTP probe through the onion (roadmap v0.5)"},
	planned{name: "gateway-separate", why: "gateways ship in v0.2"},
}

// Run executes every check for every anonymous-public service (or only for
// the named one, if service != "").
func Run(ctx context.Context, env Env, service string) []Result {
	var out []Result
	for _, svc := range env.Config.Services {
		if service != "" && svc.Name != service {
			continue
		}
		if svc.Visibility != config.VisibilityAnonymousPublic {
			continue
		}
		for _, c := range Checks {
			r := c.Run(ctx, env, svc)
			r.Check, r.Service = c.Name(), svc.Name
			out = append(out, r)
		}
	}
	return out
}

// Failed reports whether any result failed.
func Failed(rs []Result) bool {
	for _, r := range rs {
		if r.Status == StatusFail {
			return true
		}
	}
	return false
}

// noClearnet: anonymous-public services must not be reachable over clearnet.
type noClearnet struct{}

func (noClearnet) Name() string { return "no-clearnet-listener" }

func (noClearnet) Run(_ context.Context, env Env, svc config.Service) Result {
	if !env.Config.Transports.Clearnet {
		return Result{Status: StatusPass, Detail: "clearnet transport is disabled"}
	}
	for _, t := range svc.Transports {
		if t == "clearnet" {
			return Result{Status: StatusFail, Detail: "service lists clearnet in its transports"}
		}
	}
	// The node never publishes anonymous-public services on clearnet, but the
	// node's own clearnet listener still reveals this machine's IP on the LAN.
	return Result{Status: StatusFail, Detail: fmt.Sprintf("clearnet transport is enabled (listening on %s); disable it for anonymous hosting", env.Config.Transports.ClearnetListen)}
}

type planned struct{ name, why string }

func (p planned) Name() string { return p.name }

func (p planned) Run(context.Context, Env, config.Service) Result {
	return Result{Status: StatusSkip, Detail: "not checked: " + p.why}
}
