package files

import (
	"bufio"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

// ignorer decides which paths Files never syncs: its own state, editor temp files and the
// patterns in .solignore at the folder root (one glob per line, "#" comments; a pattern with a
// "/" matches the whole path, otherwise any one name in it).
type ignorer struct {
	mu       sync.Mutex
	patterns []string
	mtime    int64
	size     int64
}

const tempSuffix = ".sol-tmp"

func builtinIgnored(name string) bool {
	return strings.HasSuffix(name, ".swp") || strings.HasSuffix(name, ".swo") || strings.HasPrefix(name, "~$") ||
		strings.HasPrefix(name, ".#") || strings.HasSuffix(name, tempSuffix) || name == ".DS_Store" || name == "Thumbs.db"
}

// reload rereads .solignore when it changed.
func (g *ignorer) reload(root string) {
	st, err := os.Stat(filepath.Join(root, ".solignore"))
	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil {
		g.patterns, g.mtime, g.size = nil, 0, 0
		return
	}
	if st.ModTime().UnixNano() == g.mtime && st.Size() == g.size {
		return
	}
	g.mtime, g.size = st.ModTime().UnixNano(), st.Size()
	g.patterns = nil
	f, err := os.Open(filepath.Join(root, ".solignore"))
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		g.patterns = append(g.patterns, strings.TrimSuffix(l, "/"))
	}
}

// ignored reports whether p or any folder above it is ignored.
func (g *ignorer) ignored(p string) bool {
	if p == ".sol" || strings.HasPrefix(p, ".sol/") {
		return true
	}
	g.mu.Lock()
	pats := g.patterns
	g.mu.Unlock()
	parts := strings.Split(p, "/")
	for i, el := range parts {
		if builtinIgnored(el) {
			return true
		}
		sofar := strings.Join(parts[:i+1], "/")
		for _, pat := range pats {
			target := el
			if strings.Contains(pat, "/") {
				target = sofar
				pat = strings.TrimPrefix(pat, "/")
			}
			if ok, _ := path.Match(pat, target); ok {
				return true
			}
		}
	}
	return false
}
