package files

import (
	"strings"
	"testing"
)

const (
	h1 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	h2 = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	h3 = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
)

func put(t *Tree, dev string, seq uint64, p, blob, base string, mtime int64) {
	t.Apply(dev, "", seq, body{Op: opPut, Path: p, Blob: blob, Size: 1, Mtime: mtime, Base: base, Dev: dev})
}

func TestPathValidation(t *testing.T) {
	bad := []string{"CON", "aux.txt", "dir/NUL", "com1", "LPT9.log", "a:b", `a"b`, "a<b", "a>b", "a|b", "a?b", "a*b", `a\b`,
		"trail.", "trail /x", "a/../b", "/abs", "a//b", "", ".sol", ".sol/x", "tab\tname", strings.Repeat("x", 300)}
	for _, p := range bad {
		if err := ValidatePath(p, false); err == nil {
			t.Errorf("ValidatePath(%q) accepted", p)
		}
	}
	good := []string{"a", "Notes/ideas.md", "COM10", "日本語/ファイル.txt", "a b/c d", ".hidden", "CONSOLE"}
	for _, p := range good {
		if err := ValidatePath(p, false); err != nil {
			t.Errorf("ValidatePath(%q): %v", p, err)
		}
	}
	for in, want := range map[string]string{"/Notes//x/": "Notes/x", "./a/./b": "a/b", "café/x": "café/x", "Mañana": "Mañana"} {
		got, err := CleanPath(in, false)
		if err != nil || got != want {
			t.Errorf("CleanPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"a/../b", "..", "a\\b", "CON"} {
		if _, err := CleanPath(in, false); err == nil {
			t.Errorf("CleanPath(%q) accepted", in)
		}
	}
	if _, err := CleanPath("", true); err != nil {
		t.Error("root should be allowed")
	}
}

func TestFoldLastWriterWins(t *testing.T) {
	tr := NewTree()
	put(tr, "a", 1, "f", h1, "", 100)
	put(tr, "a", 2, "f", h2, h1, 200) // a normal edit: supersedes h1
	r := tr.Resolve()
	if len(r) != 1 || r["f"].Blob != h2 {
		t.Fatalf("got %+v", r)
	}
	if len(tr.conflicts()) != 0 {
		t.Fatal("a chain of edits is not a conflict")
	}
	// Reverting to an older content is a normal edit too.
	put(tr, "a", 3, "f", h1, h2, 300)
	if tr.Resolve()["f"].Blob != h1 || len(tr.conflicts()) != 0 {
		t.Fatal("revert to old content must win")
	}
}

func TestFoldConflictIsOrderIndependent(t *testing.T) {
	build := func(order []int) *Tree {
		tr := NewTree()
		put(tr, "a", 1, "ideas.md", h1, "", 100)
		ops := []func(){
			func() { put(tr, "a", 2, "ideas.md", h2, h1, 300) }, // laptop edits
			func() { put(tr, "b", 1, "ideas.md", h3, h1, 200) }, // phone edits the same base
		}
		for _, i := range order {
			ops[i]()
		}
		return tr
	}
	x, y := build([]int{0, 1}), build([]int{1, 0})
	for _, tr := range []*Tree{x, y} {
		r := tr.Resolve()
		if r["ideas.md"].Blob != h2 {
			t.Fatalf("later edit must keep the name: %+v", r)
		}
		var copyName string
		for p, v := range r {
			if v.Orig == "ideas.md" {
				copyName = p
			}
		}
		if !strings.HasPrefix(copyName, "ideas (conflict from b 1970-01-01 0000") || !strings.HasSuffix(copyName, ").md") {
			t.Fatalf("copy name %q", copyName)
		}
	}
	a, b := x.Resolve(), y.Resolve()
	if len(a) != len(b) {
		t.Fatal("devices disagree")
	}
	for p, v := range a {
		if b[p].Blob != v.Blob {
			t.Fatalf("devices disagree on %s", p)
		}
	}
}

func TestFoldDeleteMoveAndEditWins(t *testing.T) {
	tr := NewTree()
	put(tr, "a", 1, "d/x", h1, "", 1)
	put(tr, "a", 2, "d/y", h2, "", 1)
	tr.Apply("a", "", 3, body{Op: opMove, Path: "d", To: "e"})
	r := tr.Resolve()
	if r["e/x"].Blob != h1 || r["e/y"].Blob != h2 || r["d/x"].Blob != "" {
		t.Fatalf("move: %+v", r)
	}
	// Delete of h1 while another device edited it to h3: the edit survives.
	put(tr, "b", 1, "e/x", h3, h1, 5)
	tr.Apply("a", "", 4, body{Op: opDelete, Path: "e/x", Base: h1, Mtime: 6})
	if tr.Resolve()["e/x"].Blob != h3 {
		t.Fatal("an edit must survive a concurrent delete")
	}
	tr.Apply("a", "", 5, body{Op: opDelete, Path: "e/x", Base: h3, Mtime: 7})
	if _, ok := tr.Resolve()["e/x"]; ok {
		t.Fatal("delete with matching base removes the file")
	}
	// Invalid wire paths are ignored.
	tr.Apply("a", "", 6, body{Op: opPut, Path: "CON", Blob: h1})
	tr.Apply("a", "", 7, body{Op: opPut, Path: "../x", Blob: h1})
	if len(tr.Resolve()) != 1 {
		t.Fatalf("invalid paths leaked into the tree: %+v", tr.Resolve())
	}
}
