package netutil

import "testing"

// Online asks the routing table the same question LANIP does, so the two
// must agree that an IPv4 route exists when LANIP finds one.
func TestOnlineAgreesWithLANIP(t *testing.T) {
	if _, err := LANIP(); err == nil && !Online() {
		t.Fatal("LANIP found an outbound address but Online says there is no route")
	}
}
