package js

import (
	"io/fs"
	"os/exec"
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

// TestNodeSuite runs the plain-node tests (fetch stub, theme swap logic) when node is installed.
func TestNodeSuite(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	out, err := exec.Command(node, "test/sol.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("node test: %v\n%s", err, out)
	}
}
