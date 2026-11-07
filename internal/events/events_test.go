package events

import (
	"fmt"
	"testing"
)

func TestRingBuffer(t *testing.T) {
	l := New(3)
	if got := l.Recent(0); len(got) != 0 {
		t.Fatalf("empty log returned %d events", len(got))
	}
	for i := range 5 {
		l.Add(KindPair, fmt.Sprint(i))
	}
	got := l.Recent(0)
	if len(got) != 3 || got[0].Message != "4" || got[2].Message != "2" {
		t.Fatalf("unexpected order: %+v", got)
	}
	if got := l.Recent(1); len(got) != 1 || got[0].Message != "4" {
		t.Fatalf("Recent(1) = %+v", got)
	}
}
