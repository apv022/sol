package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// value is a decoded JSON value that keeps object key order (the app's order is usually the
// most readable one).
type value struct {
	obj  []field // object
	arr  []value // array
	raw  json.RawMessage
	kind byte // 'o', 'a' or 's' (scalar)
}

type field struct {
	key string
	val value
}

func decodeOrdered(dec *json.Decoder) (value, error) {
	t, err := dec.Token()
	if err != nil {
		return value{}, err
	}
	switch t {
	case json.Delim('{'):
		v := value{kind: 'o'}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return v, err
			}
			k, _ := kt.(string)
			fv, err := decodeOrdered(dec)
			if err != nil {
				return v, err
			}
			v.obj = append(v.obj, field{k, fv})
		}
		_, err := dec.Token()
		return v, err
	case json.Delim('['):
		v := value{kind: 'a'}
		for dec.More() {
			e, err := decodeOrdered(dec)
			if err != nil {
				return v, err
			}
			v.arr = append(v.arr, e)
		}
		_, err := dec.Token()
		return v, err
	}
	b, _ := json.Marshal(t)
	if n, ok := t.(json.Number); ok {
		b = []byte(n)
	}
	return value{kind: 's', raw: b}, nil
}

// text is how a value reads in a cell or after "key: ": strings bare, other scalars as JSON,
// lists of scalars comma separated, anything nested as compact JSON.
func (v value) text() string {
	switch v.kind {
	case 's':
		var s string
		if json.Unmarshal(v.raw, &s) == nil {
			return strings.Join(strings.Fields(s), " ")
		}
		if string(v.raw) == "null" {
			return ""
		}
		return string(v.raw)
	case 'a':
		parts := make([]string, 0, len(v.arr))
		for _, e := range v.arr {
			if e.kind != 's' {
				return v.compact()
			}
			parts = append(parts, e.text())
		}
		return strings.Join(parts, ", ")
	}
	return v.compact()
}

func (v value) compact() string {
	var b bytes.Buffer
	v.write(&b)
	return b.String()
}

func (v value) write(b *bytes.Buffer) {
	switch v.kind {
	case 's':
		b.Write(v.raw)
	case 'a':
		b.WriteByte('[')
		for i, e := range v.arr {
			if i > 0 {
				b.WriteByte(',')
			}
			e.write(b)
		}
		b.WriteByte(']')
	case 'o':
		b.WriteByte('{')
		for i, f := range v.obj {
			if i > 0 {
				b.WriteByte(',')
			}
			k, _ := json.Marshal(f.key)
			b.Write(k)
			b.WriteByte(':')
			f.val.write(b)
		}
		b.WriteByte('}')
	}
}

func (v value) tableLike() bool {
	if v.kind != 'a' || len(v.arr) == 0 {
		return false
	}
	for _, e := range v.arr {
		if e.kind != 'o' {
			return false
		}
	}
	return true
}

// Render writes an action's JSON output for people: an array of objects as a table, an
// object as "key: value" lines (a field holding an array of objects becomes a titled table
// below them), a list of scalars one per line, a scalar as itself. Anything that is not JSON
// is written as it is.
func Render(w io.Writer, out []byte) error {
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	v, err := decodeOrdered(dec)
	if err != nil {
		_, err = w.Write(out)
		return err
	}
	return render(w, v)
}

func render(w io.Writer, v value) error {
	switch {
	case v.tableLike():
		return table(w, v.arr)
	case v.kind == 'a':
		if len(v.arr) == 0 {
			_, err := fmt.Fprintln(w, "(none)")
			return err
		}
		for _, e := range v.arr {
			fmt.Fprintln(w, e.text())
		}
		return nil
	case v.kind == 'o':
		tw := newTab(w, 1)
		var tables []field
		for _, f := range v.obj {
			if f.val.tableLike() {
				tables = append(tables, f)
				continue
			}
			fmt.Fprintf(tw, "%s:\t%s\n", f.key, f.val.text())
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		for _, f := range tables {
			fmt.Fprintf(w, "\n%s:\n", f.key)
			if err := table(w, f.val.arr); err != nil {
				return err
			}
		}
		return nil
	}
	_, err := fmt.Fprintln(w, v.text())
	return err
}

// table writes rows of objects with the union of their keys as columns, in first-seen order.
func table(w io.Writer, rows []value) error {
	var cols []string
	seen := map[string]bool{}
	for _, r := range rows {
		for _, f := range r.obj {
			if !seen[f.key] {
				seen[f.key] = true
				cols = append(cols, f.key)
			}
		}
	}
	tw := newTab(w, 2)
	fmt.Fprintln(tw, strings.ToUpper(strings.Join(cols, "\t")))
	for _, r := range rows {
		cells := make([]string, len(cols))
		for i, c := range cols {
			for _, f := range r.obj {
				if f.key == c {
					cells[i] = f.val.text()
				}
			}
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	return tw.Flush()
}

// newTab returns a tabwriter whose lines lose their trailing blanks (empty last cells), so
// output has no invisible trailing whitespace.
func newTab(w io.Writer, pad int) *tabwriter.Writer {
	return tabwriter.NewWriter(&trimWriter{w: w}, 0, 4, pad, ' ', 0)
}

// trimWriter strips spaces before each newline, holding a partial line until its newline
// arrives (tabwriter writes cell by cell; every line it writes ends in one).
type trimWriter struct {
	w    io.Writer
	line []byte
}

// Write implements io.Writer.
func (t *trimWriter) Write(p []byte) (int, error) {
	for _, c := range p {
		if c != '\n' {
			t.line = append(t.line, c)
			continue
		}
		out := append(bytes.TrimRight(t.line, " "), '\n')
		t.line = t.line[:0]
		if _, err := t.w.Write(out); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}
