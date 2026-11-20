package merge

import (
	"math/rand"
	"strings"
	"testing"
)

func TestMerge(t *testing.T) {
	cases := []struct {
		name               string
		base, mine, theirs string
		want               string
		ok                 bool
	}{
		{"non-overlapping", "a\nb\nc\nd\ne\n", "A\nb\nc\nd\ne\n", "a\nb\nc\nd\nE\n", "A\nb\nc\nd\nE\n", true},
		{"identical change", "a\nb\nc\n", "a\nX\nc\n", "a\nX\nc\n", "a\nX\nc\n", true},
		{"same line differently", "a\nb\nc\n", "a\nX\nc\n", "a\nY\nc\n", "", false},
		{"adjacent lines conflict", "a\nb\nc\n", "a\nX\nc\n", "a\nb\nY\n", "", false},
		{"insert and delete apart", "1\n2\n3\n4\n5\n6\n", "1\n2\nnew\n3\n4\n5\n6\n", "1\n2\n3\n4\n6\n", "1\n2\nnew\n3\n4\n6\n", true},
		{"both insert same place", "a\nb\n", "a\nX\nb\n", "a\nY\nb\n", "", false},
		{"empty base differ", "", "x\n", "y\n", "", false},
		{"empty base identical", "", "x\n", "x\n", "x\n", true},
		{"empty base one side", "", "", "y\n", "y\n", true},
		{"one side unchanged", "a\nb\n", "a\nb\n", "a\nc\n", "a\nc\n", true},
		{"no trailing newline kept", "a\nb\nc", "A\nb\nc", "a\nb\nC", "A\nb\nC", true},
		{"no trailing newline apart", "a\nb\nc\nd", "A\nb\nc\nd", "a\nb\nc\nD", "A\nb\nc\nD", true},
		{"newline added at end", "a\nb\nc\nd", "A\nb\nc\nd", "a\nb\nc\nd\n", "A\nb\nc\nd\n", true},
		{"crlf files", "a\r\nb\r\nc\r\nd\r\n", "A\r\nb\r\nc\r\nd\r\n", "a\r\nb\r\nc\r\nD\r\n", "A\r\nb\r\nc\r\nD\r\n", true},
		{"crlf vs lf edit", "a\r\nb\r\nc\r\nd\r\n", "a\nb\nc\nd\n", "a\r\nb\r\nc\r\nD\r\n", "a\r\nb\r\nc\r\nD\r\n", true},
		{"both delete same", "a\nb\nc\n", "a\nc\n", "a\nc\n", "a\nc\n", true},
		{"delete vs edit conflict", "a\nb\nc\n", "a\nc\n", "a\nB\nc\n", "", false},
	}
	for _, c := range cases {
		got, ok := Merge([]byte(c.base), []byte(c.mine), []byte(c.theirs))
		if ok != c.ok || (ok && string(got) != c.want) {
			t.Errorf("%s: got %q ok=%v, want %q ok=%v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestIdenticalSides(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		base, side := randText(r, 30), randText(r, 30)
		got, ok := Merge([]byte(base), []byte(side), []byte(side))
		if !ok || string(got) != side {
			t.Fatalf("identical sides must return the side")
		}
	}
}

// Edits to separated regions (every edited line is at least two lines from the other side's)
// always merge, and the result has both edits.
func TestNonOverlappingRandom(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	for i := 0; i < 300; i++ {
		n := 40 + r.Intn(40)
		base := make([]string, n)
		for k := range base {
			base[k] = string(rune('a'+r.Intn(26))) + string(rune('a'+r.Intn(26))) + "\n"
		}
		mine, theirs := append([]string(nil), base...), append([]string(nil), base...)
		want := append([]string(nil), base...)
		// Even indexes in the first half belong to mine, in the second half to theirs.
		for k := 0; k < n/2-2; k += 3 {
			if r.Intn(2) == 0 {
				mine[k], want[k] = "M"+base[k], "M"+base[k]
			}
		}
		for k := n/2 + 2; k < n; k += 3 {
			if r.Intn(2) == 0 {
				theirs[k], want[k] = "T"+base[k], "T"+base[k]
			}
		}
		got, ok := Merge([]byte(strings.Join(base, "")), []byte(strings.Join(mine, "")), []byte(strings.Join(theirs, "")))
		if !ok || string(got) != strings.Join(want, "") {
			t.Fatalf("iteration %d: ok=%v\n got %q\nwant %q", i, ok, got, strings.Join(want, ""))
		}
	}
}

func TestDiffReconstructs(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	for i := 0; i < 300; i++ {
		a, b := lines([]byte(randText(r, 25))), lines([]byte(randText(r, 25)))
		hs, ok := hunks(a, b)
		if !ok {
			t.Fatal("diff failed")
		}
		if got := strings.Join(render(a, hs, 0, len(a)), ""); got != strings.Join(b, "") {
			t.Fatalf("hunks do not rebuild b: %q vs %q", got, strings.Join(b, ""))
		}
	}
}

func randText(r *rand.Rand, n int) string {
	var sb strings.Builder
	for i, k := 0, r.Intn(n); i < k; i++ {
		sb.WriteString(string(rune('a' + r.Intn(4))))
		sb.WriteByte('\n')
	}
	return sb.String()
}
