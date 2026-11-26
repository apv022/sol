package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/apv022/sol/internal/nearby"
	"github.com/apv022/sol/sdk/go/sol"
)

// sol.nearby --ble: not_implemented off Android (status "unavailable"); under the shell it is
// dry-run and set, kept in config.json, pushed as a nearby event and shown in the nearby row;
// the shell's endpoints check their input and refuse a browser session.
func TestBLEAction(t *testing.T) {
	d := testDaemon(t)
	c := adminClient(t, d)
	ctx := context.Background()
	call := func(args string, dry bool) (map[string]any, error) {
		var out map[string]any
		err := c.Do(ctx, "POST", "/admin/v1/actions/call", sol.ActionCall{Action: "sol.nearby",
			Args: json.RawMessage(args), DryRun: dry}, &out)
		return out, err
	}
	if _, err := call(`{"ble":"on"}`, false); !isErrCode(err, "not_implemented") {
		t.Fatalf("off Android: %v", err)
	}
	if out, _ := call(`{}`, false); out["ble"].(map[string]any)["state"] != "unavailable" {
		t.Fatalf("show off Android: %v", out)
	}
	if err := c.Do(ctx, "POST", "/admin/v1/nearby/ble/ad", map[string]int{"psm": 0x80}, nil); !isErrCode(err, "not_implemented") {
		t.Fatalf("ad off Android: %v", err)
	}
	if d.bt.t != nil {
		t.Fatal("a BLE transport off Android")
	}

	t.Setenv("SOL_ANDROID_SHELL", "1")
	if out, err := call(`{"ble":"on"}`, true); err != nil || out["would_set_ble"] != "on" || d.bleOn() {
		t.Fatalf("dry run: %v %v", out, err)
	}
	cur, _ := d.events.cursor("")
	out, err := call(`{"ble":"on"}`, false)
	if err != nil || out["ble"].(map[string]any)["enabled"] != true {
		t.Fatalf("on: %v %v", out, err)
	}
	if cfg, _ := LoadConfig(d.Dir()); cfg.BLE == nil || !*cfg.BLE {
		t.Fatalf("config: %+v", cfg.BLE)
	}
	found := false
	evs, _, _ := d.events.since(cur)
	for _, e := range evs {
		found = found || e.kind == EvNearby
	}
	if !found {
		t.Fatal("no nearby event")
	}
	if err := c.Do(ctx, "POST", "/admin/v1/nearby/ble/ad", map[string]int{"psm": 7}, nil); !isErrCode(err, "bad_request") {
		t.Fatalf("bad psm: %v", err)
	}
	// testDaemon started without the shell, so it has no BLE transport: honest about it.
	if err := c.Do(ctx, "POST", "/admin/v1/nearby/ble/ad", map[string]int{"psm": 0x80}, nil); !isErrCode(err, "unavailable") {
		t.Fatalf("ad without the transport: %v", err)
	}
	var plan nearby.BLEPlan
	if err := c.Do(ctx, "POST", "/admin/v1/nearby/ble/match", map[string]any{"seen": []any{}}, &plan); err != nil || len(plan.Actions) != 0 {
		t.Fatalf("match: %+v %v", plan, err)
	}
	if err := c.Do(ctx, "POST", "/admin/v1/nearby/ble/report", map[string]string{"state": "searching"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Do(ctx, "POST", "/admin/v1/nearby/ble/report", map[string]string{"state": "dancing"}, nil); !isErrCode(err, "bad_request") {
		t.Fatalf("bad state: %v", err)
	}
	if st := d.Nearby(); st.BLE == nil || st.BLE.State != "searching" {
		t.Fatalf("status %+v", st.BLE)
	}
	if row := d.nearbyRow(); row.Detail == "" || !strings.Contains(row.Detail, "Bluetooth LE searching") {
		t.Fatalf("row %+v", row)
	}
	if out, err := call(`{"ble":"off"}`, false); err != nil || out["ble"].(map[string]any)["state"] != "off" {
		t.Fatalf("off: %v %v", out, err)
	}
	cookie := login(t, d)
	root := "localhost:" + strconv.Itoa(d.UIPort())
	resp := uiReq(t, d, "POST", root, "/admin/v1/nearby/ble/match", "http://"+root, cookie, "{}", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("browser: %d", resp.StatusCode)
	}
}
