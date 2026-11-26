package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"

	"github.com/apv022/sol/internal/nearby"
	"github.com/apv022/sol/sdk/go/sol"
)

// sol.nearby --wifi_direct: not_implemented off Android; under the shell it is shown, dry-run
// and set, kept in config.json, and pushed as a nearby event (the shell follows it). The
// shell's endpoints refuse a browser session.
func TestWifiDirectAction(t *testing.T) {
	d := testDaemon(t)
	c := adminClient(t, d)
	ctx := context.Background()
	call := func(args string, dry bool) (map[string]any, error) {
		var out map[string]any
		err := c.Do(ctx, "POST", "/admin/v1/actions/call", sol.ActionCall{Action: "sol.nearby",
			Args: json.RawMessage(args), DryRun: dry}, &out)
		return out, err
	}
	if _, err := call(`{"wifi_direct":"on"}`, false); !isErrCode(err, "not_implemented") {
		t.Fatalf("off Android: %v", err)
	}
	if out, _ := call(`{}`, false); out["wifi_direct"].(map[string]any)["state"] != "unavailable" {
		t.Fatalf("show off Android: %v", out)
	}
	if err := c.Do(ctx, "POST", "/admin/v1/nearby/wifi-direct/ad", nil, nil); !isErrCode(err, "not_implemented") {
		t.Fatalf("ad off Android: %v", err)
	}

	t.Setenv("SOL_ANDROID_SHELL", "1")
	if out, err := call(`{"wifi_direct":"on"}`, true); err != nil || out["would_set_wifi_direct"] != "on" || d.wifiDirectOn() {
		t.Fatalf("dry run: %v %v", out, err)
	}
	cur, _ := d.events.cursor("")
	out, err := call(`{"wifi_direct":"on"}`, false)
	if err != nil || out["wifi_direct"].(map[string]any)["enabled"] != true {
		t.Fatalf("on: %v %v", out, err)
	}
	if cfg, _ := LoadConfig(d.Dir()); cfg.WifiDirect == nil || !*cfg.WifiDirect {
		t.Fatalf("config: %+v", cfg.WifiDirect)
	}
	found := false
	evs, _, _ := d.events.since(cur)
	for _, e := range evs {
		found = found || e.kind == EvNearby
	}
	if !found {
		t.Fatal("no nearby event")
	}
	var ad WifiDirectAd
	g := nearby.NewGroup()
	if err := c.Do(ctx, "POST", "/admin/v1/nearby/wifi-direct/ad", map[string]any{"group": g}, &ad); err != nil || ad.TXT["v"] != "1" || ad.Expires == 0 {
		t.Fatalf("ad: %+v %v", ad, err)
	}
	if err := c.Do(ctx, "POST", "/admin/v1/nearby/wifi-direct/ad", map[string]any{"group": map[string]string{"ssid": "x", "pass": "y"}}, nil); !isErrCode(err, "bad_request") {
		t.Fatalf("bad group: %v", err)
	}
	var plan nearby.Plan
	if err := c.Do(ctx, "POST", "/admin/v1/nearby/wifi-direct/match", map[string]any{"state": "none", "seen": []any{}}, &plan); err != nil || plan.Role != "none" {
		t.Fatalf("match: %+v %v", plan, err)
	}
	if err := c.Do(ctx, "POST", "/admin/v1/nearby/wifi-direct/match", map[string]any{"state": "boss"}, nil); !isErrCode(err, "bad_request") {
		t.Fatalf("bad state: %v", err)
	}
	// A page signed in to the Sol app cannot reach the shell's endpoints.
	cookie := login(t, d)
	root := "localhost:" + strconv.Itoa(d.UIPort())
	resp := uiReq(t, d, "POST", root, "/admin/v1/nearby/wifi-direct/ad", "http://"+root, cookie, "{}", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("browser: %d", resp.StatusCode)
	}
}

func isErrCode(err error, code string) bool {
	var se *sol.Error
	return errors.As(err, &se) && se.Code == code
}
