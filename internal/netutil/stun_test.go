package netutil

import "testing"

func TestExpandSTUN(t *testing.T) {
	for in, want := range map[string]string{
		"cloudflare":            "stun.cloudflare.com:3478",
		"google":                "stun.l.google.com:19302",
		"mozilla":               "stun.services.mozilla.com:3478",
		"twilio":                "global.stun.twilio.com:3478",
		"stun.example.org:3478": "stun.example.org:3478",
	} {
		if got, err := ExpandSTUN(in); err != nil || got != want {
			t.Errorf("ExpandSTUN(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "cloudflar", "stun.example.org", ":3478", "host:0", "host:99999"} {
		if _, err := ExpandSTUN(bad); err == nil {
			t.Errorf("ExpandSTUN(%q) accepted", bad)
		}
	}
}
