package daemon

import (
	"path/filepath"
	"testing"
)

func TestDefaultDirPerOS(t *testing.T) {
	for _, c := range []struct {
		goos string
		env  map[string]string
		home string
		want string
	}{
		{"linux", nil, "/home/a", "/home/a/.local/share/sol"},
		{"linux", map[string]string{"XDG_DATA_HOME": "/x/data"}, "/home/a", "/x/data/sol"},
		{"linux", map[string]string{"XDG_DATA_HOME": "relative"}, "/home/a", "/home/a/.local/share/sol"},
		{"darwin", nil, "/Users/a", "/Users/a/Library/Application Support/Sol"},
		{"windows", map[string]string{"LOCALAPPDATA": `C:\L`}, `C:\U`, `C:\L/Sol`},
		{"linux", map[string]string{"SOL_DIR": "/tmp/s"}, "/home/a", "/tmp/s"},
		{"darwin", map[string]string{"SOL_DIR": "/tmp/s"}, "/Users/a", "/tmp/s"},
	} {
		if c.goos == "linux" && c.env["XDG_DATA_HOME"] == "/x/data" && !filepath.IsAbs("/x/data") {
			continue // "/x/data" is not absolute on a Windows host
		}
		got := dirFor(c.goos, func(k string) string { return c.env[k] }, c.home)
		if c.goos == "windows" && c.env["SOL_DIR"] == "" {
			// filepath.Join uses the host separator; only the shape matters here.
			if got != c.want && got != `C:\L\Sol` {
				t.Errorf("%s: %q", c.goos, got)
			}
			continue
		}
		got = filepath.ToSlash(got) // a Windows host joins with backslashes whatever the target OS
		if got != c.want {
			t.Errorf("%s %v: %q, want %q", c.goos, c.env, got, c.want)
		}
	}
}
