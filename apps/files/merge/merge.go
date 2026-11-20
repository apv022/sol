// Package merge is a small line-based three-way merge (diff3) for text files.
//
// Invariants: the result depends only on the three inputs and on which one is "mine", so two
// devices that merge the same triple in the same order produce the same bytes. Changes that do
// not touch each other merge; identical changes on both sides merge; overlapping or touching
// different changes are a conflict. No conflict markers are ever produced: the caller falls
// back to keeping both versions. Line endings are merged as LF and written back in the
// dominant style of the inputs (CRLF only if it outnumbers LF); a missing final newline is
// treated as part of the last line, like git does.
package merge

import "strings"

// maxEdits bounds the Myers search so a pathological pair of files cannot take unbounded
// time or memory; past it the merge reports a conflict.
const maxEdits = 8000

// Merge merges the edits base->mine and base->theirs. ok is false on a conflict (or when the
// inputs are too different to diff cheaply); merged is then nil.
func Merge(base, mine, theirs []byte) (merged []byte, ok bool) {
	if string(mine) == string(theirs) {
		return append([]byte(nil), mine...), true
	}
	crlf := strings.Count(string(base)+string(mine)+string(theirs), "\r\n")
	lf := strings.Count(string(base)+string(mine)+string(theirs), "\n") - crlf
	eol := "\n"
	if crlf > lf {
		eol = "\r\n"
	}
	b, m, t := lines(base), lines(mine), lines(theirs)
	hm, ok1 := hunks(b, m)
	ht, ok2 := hunks(b, t)
	if !ok1 || !ok2 {
		return nil, false
	}
	out, ok := combine(b, hm, ht)
	if !ok {
		return nil, false
	}
	s := strings.Join(out, "")
	if eol != "\n" {
		s = strings.ReplaceAll(s, "\n", eol)
	}
	return []byte(s), true
}

// lines splits s into lines that keep their "\n" (CRLF is normalised to LF); a final line
// without a newline is kept as is.
func lines(b []byte) []string {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	var out []string
	for len(s) > 0 {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			out = append(out, s)
			break
		}
		out = append(out, s[:i+1])
		s = s[i+1:]
	}
	return out
}

// hunk replaces base[bs:be] with side lines.
type hunk struct {
	bs, be int
	with   []string
}

// hunks returns the changes that turn a into b, from a Myers diff.
func hunks(a, b []string) ([]hunk, bool) {
	// Common prefix and suffix are free and keep the Myers part small.
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	ma, ok := match(a[p:len(a)-s], b[p:len(b)-s])
	if !ok {
		return nil, false
	}
	var hs []hunk
	n, m := len(a)-p-s, len(b)-p-s
	pi, pj := -1, -1 // last matched pair
	emit := func(i, j int) {
		if i-pi > 1 || j-pj > 1 {
			hs = append(hs, hunk{bs: p + pi + 1, be: p + i, with: b[p+pj+1 : p+j]})
		}
	}
	for i := 0; i < n; i++ {
		if ma[i] >= 0 {
			emit(i, ma[i])
			pi, pj = i, ma[i]
		}
	}
	emit(n, m)
	return hs, true
}

// match runs Myers' O(ND) diff and returns, for each index of a, the matched index of b or -1.
func match(a, b []string) ([]int, bool) {
	n, m := len(a), len(b)
	ma := make([]int, n)
	for i := range ma {
		ma[i] = -1
	}
	if n == 0 || m == 0 {
		return ma, true
	}
	ids := map[string]int{}
	intern := func(s []string) []int {
		r := make([]int, len(s))
		for i, l := range s {
			id, ok := ids[l]
			if !ok {
				id = len(ids)
				ids[l] = id
			}
			r[i] = id
		}
		return r
	}
	x, y := intern(a), intern(b)
	max := n + m
	if max > maxEdits {
		max = maxEdits
	}
	off := max + 1
	v := make([]int, 2*max+3)
	var trace [][]int
	for d := 0; d <= max; d++ {
		snap := make([]int, 2*d+1)
		copy(snap, v[off-d:off+d+1])
		trace = append(trace, snap)
		for k := -d; k <= d; k += 2 {
			var px int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				px = v[off+k+1]
			} else {
				px = v[off+k-1] + 1
			}
			py := px - k
			for px < n && py < m && x[px] == y[py] {
				px++
				py++
			}
			v[off+k] = px
			if px >= n && py >= m {
				backtrack(trace, d, n, m, ma)
				return ma, true
			}
		}
	}
	return nil, false
}

func backtrack(trace [][]int, D, n, m int, ma []int) {
	x, y := n, m
	for d := D; d > 0; d-- {
		vd := trace[d]
		k := x - y
		var pk int
		if k == -d || (k != d && vd[k-1+d] < vd[k+1+d]) {
			pk = k + 1
		} else {
			pk = k - 1
		}
		px := vd[pk+d]
		py := px - pk
		xs, ys := px+1, py // delete: x advances
		if pk == k+1 {
			xs, ys = px, py+1 // insert: y advances
		}
		for x > xs && y > ys {
			x--
			y--
			ma[x] = y
		}
		x, y = px, py
	}
	for x > 0 && y > 0 {
		x--
		y--
		ma[x] = y
	}
}

// combine applies both hunk lists to base. Hunks that overlap or touch form a cluster; a
// cluster touched by one side takes that side, by both sides it must come out identical.
func combine(base []string, hm, ht []hunk) ([]string, bool) {
	var out []string
	pos, i, j := 0, 0, 0
	for i < len(hm) || j < len(ht) {
		// Start a cluster with the earliest hunk.
		var cm, ct []hunk
		take := func(side int) hunk {
			if side == 0 {
				h := hm[i]
				i++
				cm = append(cm, h)
				return h
			}
			h := ht[j]
			j++
			ct = append(ct, h)
			return h
		}
		var first hunk
		if j >= len(ht) || (i < len(hm) && hm[i].bs <= ht[j].bs) {
			first = take(0)
		} else {
			first = take(1)
		}
		lo, hi := first.bs, first.be
		for {
			switch {
			case i < len(hm) && hm[i].bs <= hi:
				if h := take(0); h.be > hi {
					hi = h.be
				}
			case j < len(ht) && ht[j].bs <= hi:
				if h := take(1); h.be > hi {
					hi = h.be
				}
			default:
				goto done
			}
		}
	done:
		out = append(out, base[pos:lo]...)
		pos = hi
		switch {
		case len(ct) == 0:
			out = append(out, render(base, cm, lo, hi)...)
		case len(cm) == 0:
			out = append(out, render(base, ct, lo, hi)...)
		default:
			a, b := render(base, cm, lo, hi), render(base, ct, lo, hi)
			if strings.Join(a, "") != strings.Join(b, "") {
				return nil, false
			}
			out = append(out, a...)
		}
	}
	return append(out, base[pos:]...), true
}

// render is base[lo:hi] with one side's hunks applied.
func render(base []string, hs []hunk, lo, hi int) []string {
	var out []string
	pos := lo
	for _, h := range hs {
		out = append(out, base[pos:h.bs]...)
		out = append(out, h.with...)
		pos = h.be
	}
	return append(out, base[pos:hi]...)
}
