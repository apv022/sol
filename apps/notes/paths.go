package notes

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// NotesDir is the folder inside the synced folder that holds every note.
const NotesDir = "Notes"

// AttachDir is the folder, next to a note, that holds its images and attachments.
const AttachDir = "attachments"

var winReserved = map[string]bool{"con": true, "prn": true, "aux": true, "nul": true}

// checkSegment refuses names that are unsafe or invalid on some OS, so the folder works on all
// of them (the same rule Files applies). Hidden names (leading dot) are refused too: they are
// Files' and editors' temp files, never notes.
func checkSegment(s string) error {
	switch {
	case s == "" || s == "." || s == "..":
		return fmt.Errorf("invalid name %q", s)
	case len(s) > 200 || !utf8.ValidString(s):
		return errors.New("name too long or not UTF-8")
	case strings.HasPrefix(s, "."):
		return fmt.Errorf("names may not start with a dot: %q", s)
	case strings.HasSuffix(s, ".") || strings.HasSuffix(s, " "):
		return fmt.Errorf("names may not end with a dot or space: %q", s)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`<>:"\|?*`, r) {
			return fmt.Errorf("name has a character that is not allowed: %q", s)
		}
	}
	base := s
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	b := strings.ToLower(base)
	if winReserved[b] || (len(b) == 4 && (strings.HasPrefix(b, "com") || strings.HasPrefix(b, "lpt")) && b[3] >= '1' && b[3] <= '9') {
		return fmt.Errorf("reserved name %q", s)
	}
	return nil
}

// cleanRel validates a path relative to the Notes folder: "/"-separated, no "..", no leading
// slash, every segment safe. The empty string is the Notes folder itself, allowed only when
// allowRoot is set.
func cleanRel(p string, allowRoot bool) (string, error) {
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		if allowRoot {
			return "", nil
		}
		return "", errors.New("empty path")
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, `\`) || strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("invalid path %q", p)
	}
	for _, s := range strings.Split(p, "/") {
		if err := checkSegment(s); err != nil {
			return "", err
		}
	}
	return p, nil
}

// cleanNote is cleanRel for a note: it must end in .md.
func cleanNote(p string) (string, error) {
	p, err := cleanRel(p, false)
	if err != nil {
		return "", err
	}
	if !isNoteName(p) {
		return "", fmt.Errorf("notes end in .md: %q", p)
	}
	return p, nil
}

func isNoteName(p string) bool {
	return len(p) > 3 && strings.EqualFold(p[len(p)-3:], ".md")
}

func baseOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func dirOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

func join(d, n string) string {
	if d == "" {
		return n
	}
	return d + "/" + n
}

// stemOf is the file name without its folder and .md.
func stemOf(p string) string {
	b := baseOf(p)
	if isNoteName(b) {
		b = b[:len(b)-3]
	}
	return b
}

// abs maps a validated relative path to the real path on disk and refuses to leave Notes/ even
// through a symlink: the deepest existing ancestor is resolved and must stay inside the
// resolved Notes folder.
func (a *App) abs(rel string) (string, error) {
	full := filepath.Join(a.notes, filepath.FromSlash(rel))
	root, err := filepath.EvalSymlinks(a.notes)
	if err != nil {
		return "", err
	}
	cur := full
	for {
		real, err := filepath.EvalSymlinks(cur)
		if err == nil {
			if real != root && !strings.HasPrefix(real, root+string(filepath.Separator)) {
				return "", errors.New("path leaves the Notes folder")
			}
			return full, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		next := filepath.Dir(cur)
		if next == cur {
			return "", errors.New("path leaves the Notes folder")
		}
		cur = next
	}
}

// sanitizeName turns free text (a title) into a safe file stem.
func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r < 0x20 || r == 0x7f || strings.ContainsRune(`<>:"/\|?*`, r):
			b.WriteByte('-')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), " .-")
	if r := []rune(out); len(r) > 100 {
		out = strings.TrimSpace(string(r[:100]))
	}
	if out == "" || checkSegment(out+".md") != nil {
		return "Untitled"
	}
	return out
}
