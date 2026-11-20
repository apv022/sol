package files

import (
	"strings"
	"testing"
	"time"
)

// twoOfflineEdits sets up alice's two devices with base content in p, then b edits offline
// (older) and a edits (newer); b comes back online.
func twoOfflineEdits(t *testing.T, p, base, aText, bText string) (*node, *node, *world) {
	t.Helper()
	w := newWorld()
	a := startNode(t, w.a1, nil)
	b := startNode(t, w.a2, nil)
	a.write(t, p, base)
	waitFor(t, "b has base", func() bool { return b.has(p, base) })
	a.settle(t)
	b.settle(t)
	b.api.down.Store(true)
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	a.writeAt(t, p, aText, t0.Add(2*time.Minute))
	b.writeAt(t, p, bText, t0.Add(time.Minute))
	waitFor(t, "a published", func() bool { return len(treeEntries(t, a.api.API, w.alice.ID)) >= 2 })
	b.api.down.Store(false)
	return a, b, w
}

func TestMergeDifferentLines(t *testing.T) {
	base := "# Title\n\nline one\nline two\nline three\nline four\nline five\n"
	aText := strings.Replace(base, "line one", "line ONE", 1)
	bText := strings.Replace(base, "line five", "line FIVE", 1)
	want := strings.Replace(aText, "line five", "line FIVE", 1)
	a, b, _ := twoOfflineEdits(t, "Notes/n.md", base, aText, bText)
	waitFor(t, "both converge to the merge", func() bool { return a.has("Notes/n.md", want) && b.has("Notes/n.md", want) })
	a.settle(t)
	b.settle(t)
	for _, n := range []*node{a, b} {
		if got := strings.Join(n.files(), ","); got != "Notes/n.md" {
			t.Fatalf("no conflict copy expected, files: %s", got)
		}
		if n.has("Notes/n.md", want) != true {
			t.Fatal("merge lost after settling")
		}
	}
}

func TestMergeSameLineStaysConflict(t *testing.T) {
	base := "one\ntwo\nthree\nfour\nfive\n"
	a, b, _ := twoOfflineEdits(t, "n.md", base, "one\nTWO-A\nthree\nfour\nfive\n", "one\nTWO-B\nthree\nfour\nfive\n")
	waitFor(t, "conflict copy on both", func() bool {
		for _, n := range []*node{a, b} {
			fs := n.files()
			if len(fs) != 2 || !n.has("n.md", "one\nTWO-A\nthree\nfour\nfive\n") {
				return false
			}
		}
		return true
	})
}

func TestMergeBinaryStaysConflict(t *testing.T) {
	base := "a\x00b\nc\nd\ne\nf\n"
	a, b, _ := twoOfflineEdits(t, "x.dat", base, "A\x00b\nc\nd\ne\nf\n", "a\x00b\nc\nd\ne\nF\n")
	waitFor(t, "conflict copy on both", func() bool { return len(a.files()) == 2 && len(b.files()) == 2 })
}
