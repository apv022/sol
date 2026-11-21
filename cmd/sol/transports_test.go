package main

import (
	"strings"
	"testing"
)

// --transport is repeatable and comma-separated; none means the daemon's defaults; --listen
// applies to every listening transport; unknown names are refused.
func TestTransportConfigs(t *testing.T) {
	if tcs, err := transportConfigs(nil, ""); err != nil || tcs != nil {
		t.Fatalf("no flags: %+v %v", tcs, err)
	}
	tcs, err := transportConfigs([]string{"wg,direct", "tor", "wg"}, "0.0.0.0:7700")
	if err != nil || len(tcs) != 3 {
		t.Fatalf("%+v %v", tcs, err)
	}
	for _, tc := range tcs {
		want := "0.0.0.0:7700"
		if tc.Name == "tor" {
			want = ""
		}
		if tc.Listen != want {
			t.Fatalf("%s listens on %q", tc.Name, tc.Listen)
		}
	}
	tcs, err = transportConfigs([]string{"direct"}, "")
	if err != nil || len(tcs) != 1 || !strings.HasPrefix(tcs[0].Listen, "0.0.0.0:") {
		t.Fatalf("direct alone: %+v %v", tcs, err)
	}
	if _, err := transportConfigs([]string{"carrier-pigeon"}, ""); err == nil {
		t.Fatal("an unknown transport was accepted")
	}
}
