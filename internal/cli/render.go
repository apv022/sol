package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
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
func Render(w io.Writer, out []byte) error { return RenderColumns(w, out, nil) }

// RenderColumns is Render with an action's "columns" hint: tables show those fields, in that
// order (a table that has none of them falls back to the default, so a hint for one table of a
// result doesn't blank another). Without a hint a table shows the first maxCols scalar fields.
// Times (mtime, *_at, ...) and sizes (size, *_bytes) are formatted for reading; --json output
// is never touched.
func RenderColumns(w io.Writer, out []byte, columns []string) error {
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	v, err := decodeOrdered(dec)
	if err != nil {
		_, err = w.Write(out)
		return err
	}
	return render(w, v, columns)
}

func render(w io.Writer, v value, columns []string) error {
	switch {
	case v.tableLike():
		return table(w, v.arr, columns)
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
			fmt.Fprintf(tw, "%s:\t%s\n", f.key, f.val.cell(f.key))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		for _, f := range tables {
			fmt.Fprintf(w, "\n%s:\n", f.key)
			if err := table(w, f.val.arr, columns); err != nil {
				return err
			}
		}
		return nil
	}
	_, err := fmt.Fprintln(w, v.text())
	return err
}

// maxCols is how many columns a table without a hint shows.
const maxCols = 5

// pickColumns chooses a table's columns: the hint's fields that occur in the rows, else the
// first maxCols fields that are scalars (nested objects and lists don't fit a cell).
func pickColumns(rows []value, hint []string) []string {
	var all, scalar []string
	seen := map[string]bool{}
	for _, r := range rows {
		for _, f := range r.obj {
			if !seen[f.key] {
				seen[f.key] = true
				all = append(all, f.key)
			}
		}
	}
	var cols []string
	for _, h := range hint {
		if seen[h] {
			cols = append(cols, h)
		}
	}
	if len(cols) > 0 {
		return cols
	}
	for _, k := range all {
		for _, r := range rows {
			if f, ok := r.field(k); ok && f.kind == 's' {
				scalar = append(scalar, k)
				break
			}
		}
	}
	if len(scalar) == 0 {
		scalar = all
	}
	return scalar[:min(len(scalar), maxCols)]
}

func (v value) field(k string) (value, bool) {
	for _, f := range v.obj {
		if f.key == k {
			return f.val, true
		}
	}
	return value{}, false
}

// table writes rows of objects, one column per picked field.
func table(w io.Writer, rows []value, hint []string) error {
	cols := pickColumns(rows, hint)
	tw := newTab(w, 2)
	fmt.Fprintln(tw, strings.ToUpper(strings.Join(cols, "\t")))
	for _, r := range rows {
		cells := make([]string, len(cols))
		for i, c := range cols {
			if f, ok := r.field(c); ok {
				cells[i] = f.cell(c)
			}
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	return tw.Flush()
}

// cell is text, with numbers under time-like and size-like keys made readable.
func (v value) cell(key string) string {
	if v.kind != 's' {
		return v.text()
	}
	n, err := strconv.ParseInt(string(v.raw), 10, 64)
	if err != nil {
		return v.text()
	}
	k := strings.ToLower(key)
	switch {
	case k == "size" || strings.HasSuffix(k, "_size") || strings.HasSuffix(k, "_bytes") || k == "bytes":
		return humanSize(n)
	case n > 0 && (k == "mtime" || k == "ctime" || k == "time" || k == "ts" || strings.HasSuffix(k, "_at") || strings.HasSuffix(k, "_time") || strings.HasSuffix(k, "_ms")):
		return humanTime(n)
	}
	return v.text()
}

// humanSize writes n bytes as "512 B", "1.5 KiB", ...
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	f, i := float64(n), 0
	for ; f >= unit && i < 5; i++ {
		f /= unit
	}
	return fmt.Sprintf("%.1f %ciB", f, "KMGTPE"[i-1])
}

// humanTime writes a Unix time in seconds or milliseconds (told apart by magnitude) in local
// time, "2006-01-02 15:04".
func humanTime(n int64) string {
	t := time.Unix(n, 0)
	if n > 1e11 {
		t = time.UnixMilli(n)
	}
	return t.Format("2006-01-02 15:04")
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

// CallSummary is one line naming an inbox approval's call for `sol inbox`: the action and
// its args as compact JSON, cut to max runes. The daemon already shows secret values as
// sol.Redacted, so this never sees them; an answered approval has no args left to show.
func CallSummary(action string, args json.RawMessage, max int) string {
	s := action
	var b bytes.Buffer
	if len(args) > 0 && json.Compact(&b, args) == nil && b.String() != "{}" {
		s += " " + b.String()
	}
	if r := []rune(s); max > 1 && len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}
