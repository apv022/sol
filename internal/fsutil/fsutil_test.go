package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileAtomic(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	for _, s := range []string{"one", "two"} {
		if err := WriteFileAtomic(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(p)
		if string(b) != s {
			t.Fatalf("got %q", b)
		}
	}
	ents, _ := os.ReadDir(filepath.Dir(p))
	if len(ents) != 1 {
		t.Fatalf("temp file left behind: %v", ents)
	}
}
