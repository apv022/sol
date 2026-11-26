package sol

import (
	"fmt"
	"testing"
	"time"
)

func TestBlobRefsRelease(t *testing.T) {
	a, b := "sha256:"+fmt.Sprintf("%064x", 1), "sha256:"+fmt.Sprintf("%064x", 2)
	refs, rels := BlobRefs([]byte(`{"blob":"` + a + `","release":["` + a + `","` + b + `"],"x":{"release":"` + b + `"}}`))
	// a is referenced elsewhere in the entry (wins); b is also named in a nested, ordinary field.
	if fmt.Sprint(refs) != fmt.Sprint([]string{a, b}) || len(rels) != 0 {
		t.Fatalf("refs %v, releases %v", refs, rels)
	}
	refs, rels = BlobRefs([]byte(`{"op":"purge","release":["` + a + `"]}`))
	if len(refs) != 0 || fmt.Sprint(rels) != fmt.Sprint([]string{a}) {
		t.Fatalf("refs %v, releases %v", refs, rels)
	}
}

func TestBlobIndex(t *testing.T) {
	h := "sha256:" + fmt.Sprintf("%064x", 7)
	at := time.UnixMilli(1000)
	e := func(feed, dev string, seq uint64, ms int64, body string) Entry {
		return Entry{Feed: feed, Device: dev, Seq: seq, Time: at.Add(time.Duration(ms) * time.Millisecond), Body: []byte(body)}
	}
	var x BlobIndex
	x.Add(e("f", "d1", 1, 0, `{"blob":"`+h+`"}`))
	x.Add(e("g", "d1", 1, 0, `{"blob":"`+h+`"}`))
	if rel := x.Add(e("f", "d2", 1, 5, `{"release":["`+h+`"]}`)); len(rel) != 1 {
		t.Fatalf("Add returned %v", rel)
	}
	if x.Referenced("f", h) || !x.Released("f", h) || fmt.Sprint(x.Feeds(h)) != "[g]" || len(x.Hashes("f")) != 0 {
		t.Fatal("release not applied")
	}
	// An older reference arriving late does not undo the release; a newer one does.
	x.Add(e("f", "d3", 1, 2, `{"blob":"`+h+`"}`))
	if x.Referenced("f", h) {
		t.Fatal("an older reference undid the release")
	}
	x.Add(e("f", "d1", 2, 6, `{"blob":"`+h+`"}`))
	if !x.Referenced("f", h) || x.Released("f", h) || fmt.Sprint(x.Hashes("f")) != "["+h+"]" {
		t.Fatal("a newer reference does not count")
	}
}
