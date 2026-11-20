package js

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// unsafeInner flags assignments of template literals or concatenations to innerHTML.
var unsafeInner = regexp.MustCompile("innerHTML\\s*\\+?=\\s*(`|.*\\+|.*\\$\\{)")

func TestEmbedAndNoUnsafeInnerHTML(t *testing.T) {
	b, err := fs.ReadFile(FS, "sol.js")
	if err != nil || len(b) == 0 {
		t.Fatalf("sol.js not embedded: %v", err)
	}
	for i, line := range strings.Split(string(b), "\n") {
		if unsafeInner.MatchString(line) {
			t.Errorf("sol.js:%d: innerHTML assigned from a template or concatenation: %s", i+1, line)
		}
	}
	if !strings.Contains(string(b), "export function html") {
		t.Error("sol.js must export html")
	}
}
