package kit

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var unsafeInner = regexp.MustCompile("innerHTML\\s*\\+?=\\s*(`|.*\\+|.*\\$\\{)")

func TestEmbedAndNoUnsafeInnerHTML(t *testing.T) {
	for _, name := range []string{"tokens.css", "kit.css", "kit.js", "icons.js", "demo.html", "TOKENS.md", "fonts/InterVariable.woff2", "fonts/OFL.txt", "fonts/VENDORED.txt", "themes/sol.css", "themes/graphite.css", "themes/terminal.css", "themes/paper.css"} {
		b, err := fs.ReadFile(FS, name)
		if err != nil || len(b) == 0 {
			t.Fatalf("%s not embedded: %v", name, err)
		}
		if !strings.HasSuffix(name, ".js") && !strings.HasSuffix(name, ".html") {
			continue
		}
		for i, line := range strings.Split(string(b), "\n") {
			if unsafeInner.MatchString(line) {
				t.Errorf("%s:%d: innerHTML assigned from a template or concatenation", name, i+1)
			}
		}
	}
	icons, _ := fs.ReadFile(FS, "icons.js")
	if n := strings.Count(string(icons), "\n  "); n < 30 {
		t.Errorf("want at least 30 icons, got %d", n)
	}
}

var hexColour = regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`)
var rgbColour = regexp.MustCompile(`\b(rgba?|hsla?)\(`)

// App UI stylesheets style through tokens only: a hard-coded colour would ignore the theme.
func TestAppCSSUsesTokensOnly(t *testing.T) {
	files, err := filepath.Glob("../../apps/*/ui/*.css")
	if err != nil || len(files) < 2 {
		t.Fatalf("expected app stylesheets, got %v %v", files, err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if hexColour.MatchString(line) || rgbColour.MatchString(line) {
				t.Errorf("%s:%d: hard-coded colour (use a token): %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// Built-in themes override tokens only and carry the documented header.
func TestBuiltinThemes(t *testing.T) {
	for _, n := range []string{"sol", "graphite", "terminal", "paper"} {
		b, err := fs.ReadFile(FS, "themes/"+n+".css")
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if !strings.Contains(s, "@theme") || !strings.Contains(s, "@author") || !strings.Contains(s, "@supports") {
			t.Errorf("%s.css: missing header", n)
		}
		if strings.Contains(s, "@import") || strings.Contains(s, "url(") {
			t.Errorf("%s.css: themes must not load anything", n)
		}
	}
}

// Old WebViews (Chromium < 123) drop a whole declaration they cannot parse, which once turned dark
// mode into plain white-on-black. The kit therefore never uses light-dark(), and every color-mix()
// or oklch() has a solid fallback for the same property declared earlier in the rule, or sits in
// an @supports block.
func TestKitCSSOldBrowserSafe(t *testing.T) {
	modern := regexp.MustCompile(`color-mix\(|oklch\(`)
	comment := regexp.MustCompile(`(?s)/\*.*?\*/`)
	prop := regexp.MustCompile(`([-a-zA-Z]+)\s*:`)
	for _, name := range []string{"tokens.css", "kit.css"} {
		b, err := fs.ReadFile(FS, name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "light-dark(") {
			t.Errorf("%s uses light-dark() (needs Chrome 123+)", name)
		}
		src := comment.ReplaceAllStringFunc(string(b), func(c string) string { return strings.Repeat("\n", strings.Count(c, "\n")) })
		var ruleDecls []string // property names declared so far in each open block
		var supports []bool    // whether each open block is (inside) an @supports
		for i, line := range strings.Split(src, "\n") {
			inSupports := strings.Contains(line, "@supports")
			for _, s := range supports {
				inSupports = inSupports || s
			}
			if modern.MatchString(line) && !inSupports {
				// the property being set on this line, and whether an earlier line of the rule set it
				m := prop.FindStringSubmatch(line)
				ok := false
				if m != nil && len(ruleDecls) > 0 {
					for _, d := range strings.Split(ruleDecls[len(ruleDecls)-1], " ") {
						ok = ok || d == m[1]
					}
				}
				if !ok {
					t.Errorf("%s:%d: color-mix()/oklch() without an earlier fallback in the same rule: %s", name, i+1, strings.TrimSpace(line))
				}
			}
			// track braces and declarations (line based: good enough for this file's formatting)
			code := line
			for _, m := range prop.FindAllStringSubmatch(code, -1) {
				if len(ruleDecls) > 0 && !strings.HasPrefix(strings.TrimSpace(code), "@") {
					ruleDecls[len(ruleDecls)-1] += " " + m[1]
				}
			}
			for _, c := range code {
				switch c {
				case '{':
					supports = append(supports, strings.Contains(code, "@supports"))
					ruleDecls = append(ruleDecls, "")
				case '}':
					if len(supports) > 0 {
						supports = supports[:len(supports)-1]
						ruleDecls = ruleDecls[:len(ruleDecls)-1]
					}
				}
			}
		}
	}
}
