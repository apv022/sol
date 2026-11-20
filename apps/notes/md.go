package notes

import (
	"regexp"
	"strings"
	"unicode"
)

var (
	wikiRe  = regexp.MustCompile(`\[\[([^\[\]\n]+?)\]\]`)
	tagRe   = regexp.MustCompile(`(^|[\s(\[])#([\p{L}\p{N}_][\p{L}\p{N}_/-]*)`)
	h1Re    = regexp.MustCompile(`^#[ \t]+(.+?)[ \t#]*$`)
	fenceRe = regexp.MustCompile("^ {0,3}(```+|~~~+)")
	taskRe  = regexp.MustCompile(`^(\s*(?:[-*+]|\d+[.)])\s+\[)([ xX])(\].*)$`)
)

// noteInfo is what the index needs to know about one note's text.
type noteInfo struct {
	Title string
	Tags  []string // lowercase, unique, in order of appearance
	Links []string // raw wiki-link targets (before "|" and "#"), in order, unique by lowercase
	Snip  string   // first line of prose, for list previews
}

// stripCode blanks inline code spans so tags and links inside them are not seen.
func stripCode(line string) string {
	if !strings.Contains(line, "`") {
		return line
	}
	b := []byte(line)
	for i := 0; i < len(b); {
		if b[i] != '`' {
			i++
			continue
		}
		n := 0
		for i+n < len(b) && b[i+n] == '`' {
			n++
		}
		end := strings.Index(string(b[i+n:]), strings.Repeat("`", n))
		if end < 0 {
			i += n
			continue
		}
		for j := i; j < i+n+end+n; j++ {
			b[j] = ' '
		}
		i += n + end + n
	}
	return string(b)
}

// bodyLines yields the lines of text that are prose (outside the front matter and fenced code),
// with their index in the file. Inline code is blanked.
func eachProseLine(text string, f func(i int, line string)) {
	lines := strings.Split(text, "\n")
	start := 0
	if len(lines) > 0 && strings.TrimRight(lines[0], "\r ") == "---" {
		for j := 1; j < len(lines); j++ {
			if t := strings.TrimRight(lines[j], "\r "); t == "---" || t == "..." {
				start = j + 1
				break
			}
		}
	}
	fence := ""
	for i := start; i < len(lines); i++ {
		l := strings.TrimRight(lines[i], "\r")
		if m := fenceRe.FindStringSubmatch(l); m != nil {
			if fence == "" {
				fence = m[1][:1]
			} else if m[1][:1] == fence {
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		f(i, stripCode(l))
	}
}

// parseNote extracts title, tags and links. The title is the first "# " heading, else the file
// name without .md.
func parseNote(path, text string) noteInfo {
	info := noteInfo{Title: stemOf(path)}
	haveTitle := false
	seenTag, seenLink := map[string]bool{}, map[string]bool{}
	eachProseLine(text, func(_ int, l string) {
		if !haveTitle {
			if m := h1Re.FindStringSubmatch(l); m != nil {
				info.Title, haveTitle = strings.TrimSpace(m[1]), true
				return
			}
		}
		if info.Snip == "" {
			t := strings.TrimSpace(l)
			if t != "" && !strings.HasPrefix(t, "#") && !strings.HasPrefix(t, "---") {
				info.Snip = clip(t, 140)
			}
		}
		for _, m := range wikiRe.FindAllStringSubmatch(l, -1) {
			t := linkTarget(m[1])
			if k := strings.ToLower(t); t != "" && !seenLink[k] {
				seenLink[k] = true
				info.Links = append(info.Links, t)
			}
		}
		for _, m := range tagRe.FindAllStringSubmatch(l, -1) {
			t := strings.ToLower(strings.TrimRight(m[2], "/-"))
			if t == "" || allDigits(t) || seenTag[t] {
				continue
			}
			seenTag[t] = true
			info.Tags = append(info.Tags, t)
		}
	})
	if info.Title == "" {
		info.Title = stemOf(path)
	}
	return info
}

func allDigits(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// linkTarget is the note a wiki link names: "[[Title|alias]]" and "[[Title#heading]]" both name Title.
func linkTarget(inner string) string {
	if i := strings.IndexByte(inner, '|'); i >= 0 {
		inner = inner[:i]
	}
	if i := strings.IndexByte(inner, '#'); i >= 0 {
		inner = inner[:i]
	}
	return strings.TrimSpace(inner)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}

// rewriteLinks replaces wiki-link targets for which repl returns a new target, leaving aliases
// and heading parts alone and never touching code. It reports whether anything changed.
func rewriteLinks(text string, repl func(target string) (string, bool)) (string, bool) {
	lines := strings.Split(text, "\n")
	changed := false
	prose := map[int]bool{}
	eachProseLine(text, func(i int, _ string) { prose[i] = true })
	for i, l := range lines {
		if !prose[i] || !strings.Contains(l, "[[") {
			continue
		}
		masked := stripCode(strings.TrimRight(l, "\r"))
		var out strings.Builder
		last := 0
		for _, m := range wikiRe.FindAllStringSubmatchIndex(masked, -1) {
			inner := l[m[2]:m[3]]
			tail := ""
			core := inner
			if j := strings.IndexAny(inner, "|#"); j >= 0 {
				core, tail = inner[:j], inner[j:]
			}
			trim := strings.TrimSpace(core)
			if n, ok := repl(trim); ok && trim != "" {
				lead := core[:len(core)-len(strings.TrimLeft(core, " \t"))]
				out.WriteString(l[last:m[2]])
				out.WriteString(lead + n + tail)
				last = m[3]
				changed = true
			}
		}
		if last > 0 {
			out.WriteString(l[last:])
			lines[i] = out.String()
		}
	}
	return strings.Join(lines, "\n"), changed
}

// toggleTask flips (or sets) the checkbox of the task item on line n (0-based). It fails when
// that line is not a task item, which protects against a stale line number.
func toggleTask(text string, n int, want *bool) (string, bool, bool) {
	lines := strings.Split(text, "\n")
	if n < 0 || n >= len(lines) {
		return text, false, false
	}
	l := strings.TrimRight(lines[n], "\r")
	m := taskRe.FindStringSubmatch(l)
	if m == nil {
		return text, false, false
	}
	now := m[2] == " " // toggle: an open box becomes checked
	if want != nil {
		now = *want
	}
	nc := " "
	if now {
		nc = "x"
	}
	lines[n] = m[1] + nc + m[3] + lines[n][len(l):]
	return strings.Join(lines, "\n"), nc == "x", true
}

// tokenize splits text into lowercase search tokens (letters and digits, any script).
func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}
