package kit

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

var unsafeInner = regexp.MustCompile("innerHTML\\s*\\+?=\\s*(`|.*\\+|.*\\$\\{)")

func TestEmbedAndNoUnsafeInnerHTML(t *testing.T) {
	for _, name := range []string{"tokens.css", "kit.css", "kit.js", "icons.js", "demo.html"} {
		b, err := fs.ReadFile(FS, name)
		if err != nil || len(b) == 0 {
			t.Fatalf("%s not embedded: %v", name, err)
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
