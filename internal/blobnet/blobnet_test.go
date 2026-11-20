package blobnet

import (
	"errors"
	"testing"

	"github.com/apv022/sol/internal/blob"
)

func TestParseRange(t *testing.T) {
	for _, tc := range []struct {
		v           string
		size        int64
		off, n      int64
		ranged, bad bool
	}{
		{"", 100, 0, 100, false, false},
		{"bytes=0-9", 100, 0, 10, true, false},
		{"bytes=90-200", 100, 90, 10, true, false},
		{"bytes=95-", 100, 95, 5, true, false},
		{"bytes=-10", 100, 90, 10, true, false},
		{"bytes=-500", 100, 0, 100, true, false},
		{"bytes=100-", 100, 0, 0, true, true},
		{"bytes=5-4", 100, 0, 0, true, true},
		{"bytes=0-1,4-5", 100, 0, 0, true, true},
		{"bytes=-0", 100, 0, 0, true, true},
		{"items=0-1", 100, 0, 0, true, true},
		{"bytes=0-", 0, 0, 0, true, true},
	} {
		off, n, ranged, err := parseRange(tc.v, tc.size)
		if (err != nil) != tc.bad || (!tc.bad && (off != tc.off || n != tc.n || ranged != tc.ranged)) {
			t.Errorf("parseRange(%q, %d) = %d, %d, %v, %v", tc.v, tc.size, off, n, ranged, err)
		}
	}
}

func TestParseHash(t *testing.T) {
	h := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	for _, ok := range []string{h, blob.Ref(h)} {
		if got, err := ParseHash(ok); err != nil || got != h {
			t.Errorf("ParseHash(%q) = %q, %v", ok, got, err)
		}
	}
	for _, bad := range []string{"", "sha256:", "sha256:" + h[:10], "SHA256:" + h, h + "0"} {
		if _, err := ParseHash(bad); !errors.Is(err, ErrBadRef) {
			t.Errorf("ParseHash(%q) accepted", bad)
		}
	}
}
