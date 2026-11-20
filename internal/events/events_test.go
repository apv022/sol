package events

import "testing"

func TestFanOutNeverBlocks(t *testing.T) {
	var b Bus[int]
	fast, slow := b.Subscribe(100), b.Subscribe(1)
	for i := range 10 {
		b.Publish(i) // must not block on slow
	}
	if len(fast.C()) != 10 {
		t.Fatalf("fast got %d events", len(fast.C()))
	}
	if got := <-slow.C(); got != 0 || slow.Dropped() != 9 {
		t.Fatalf("slow got %d, dropped %d", got, slow.Dropped())
	}
	slow.Close()
	slow.Close()
	b.Publish(1)
	if _, ok := <-slow.C(); ok {
		t.Fatal("closed subscription still delivers")
	}
}
