package files

import (
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Paths in the tree are "/"-separated, NFC-normalised, relative, without "..", and valid on
// Windows even when the machine is not Windows, so the same folder works on every device.

const (
	maxElement = 255
	maxPath    = 1024
)

var reservedNames = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true}

func init() {
	for i := '1'; i <= '9'; i++ {
		reservedNames["COM"+string(i)] = true
		reservedNames["LPT"+string(i)] = true
	}
}

// ValidatePath checks one tree path exactly as it must appear on the wire: no normalisation
// is attempted. The empty path (the root) is valid only when allowRoot is set.
func ValidatePath(p string, allowRoot bool) error {
	if p == "" {
		if allowRoot {
			return nil
		}
		return fmt.Errorf("path is empty")
	}
	if len(p) > maxPath {
		return fmt.Errorf("path is longer than %d bytes", maxPath)
	}
	if !utf8.ValidString(p) {
		return fmt.Errorf("path is not valid UTF-8")
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("path must be relative")
	}
	if nfc(p) != p {
		return fmt.Errorf("path is not NFC-normalised")
	}
	for _, el := range strings.Split(p, "/") {
		if err := validElement(el); err != nil {
			return err
		}
	}
	if (p == ".sol" || strings.HasPrefix(p, ".sol/")) && !syncedSol(p) {
		return fmt.Errorf("%q is reserved", ".sol")
	}
	return nil
}

// syncedSol reports whether p is one of the few files under ".sol" that Files syncs between a
// person's devices: the active theme's name and the person's own theme files (design/UI.md
// "Themes"). Everything else under ".sol" is device state and never leaves the folder.
func syncedSol(p string) bool {
	if p == ".sol/theme" {
		return true
	}
	n, ok := strings.CutPrefix(p, ".sol/themes/")
	n, isCSS := strings.CutSuffix(n, ".css")
	return ok && isCSS && themeFileRe.MatchString(n)
}

// themeFileRe is the daemon's theme name rule (internal/daemon/theme.go); apps cannot import it.
var themeFileRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// solContainer reports whether p is a folder that holds synced ".sol" files. Such folders are
// walked but never published as folders themselves.
func solContainer(p string) bool { return p == ".sol" || p == ".sol/themes" }

func validElement(el string) error {
	switch {
	case el == "":
		return fmt.Errorf("empty name in path")
	case el == "." || el == "..":
		return fmt.Errorf("%q is not allowed in a path", el)
	case len(el) > maxElement:
		return fmt.Errorf("name %q is longer than %d bytes", el[:20]+"...", maxElement)
	}
	for _, r := range el {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`<>:"\|?*`, r) {
			return fmt.Errorf("name %q has a character that Windows does not allow (%q)", el, r)
		}
	}
	if strings.HasSuffix(el, ".") || strings.HasSuffix(el, " ") {
		return fmt.Errorf("name %q ends with a dot or a space, which Windows does not allow", el)
	}
	stem := el
	if i := strings.IndexByte(el, '.'); i >= 0 {
		stem = el[:i]
	}
	if reservedNames[strings.ToUpper(strings.TrimRight(stem, " "))] {
		return fmt.Errorf("name %q is reserved on Windows", el)
	}
	return nil
}

// CleanPath turns a path typed by a person or an agent into the canonical form: leading
// slashes dropped, "." and duplicate slashes removed, NFC applied, then validated. ".." is
// refused, never resolved.
func CleanPath(p string, allowRoot bool) (string, error) {
	p = strings.TrimLeft(p, "/")
	p = strings.TrimRight(p, "/")
	if strings.Contains(p, "\\") {
		return "", fmt.Errorf("name has a backslash, which Windows does not allow")
	}
	for _, el := range strings.Split(p, "/") {
		if el == ".." {
			return "", fmt.Errorf("%q is not allowed in a path", "..")
		}
	}
	if p != "" {
		p = path.Clean(p)
		if p == "." {
			p = ""
		}
	}
	p = nfc(p)
	if err := ValidatePath(p, allowRoot); err != nil {
		return "", err
	}
	return p, nil
}

// under reports whether p is dir or inside dir.
func under(p, dir string) bool {
	return dir == "" || p == dir || strings.HasPrefix(p, dir+"/")
}

// rel returns p relative to dir (p must be under dir).
func rel(p, dir string) string {
	if dir == "" {
		return p
	}
	return strings.TrimPrefix(strings.TrimPrefix(p, dir), "/")
}

func join(dir, name string) string {
	if dir == "" {
		return name
	}
	if name == "" {
		return dir
	}
	return dir + "/" + name
}

func parentOf(p string) string {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return ""
	}
	return p[:i]
}

func baseOf(p string) string { return p[strings.LastIndexByte(p, '/')+1:] }

// nfc composes the combining marks that matter for everyday Latin text (macOS hands out NFD
// names). It is a deliberate subset of Unicode NFC, because the standard library has no
// normaliser and Sol takes no third-party dependencies; names outside the subset are kept as
// they are.
func nfc(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return s
	}
	in := []rune(s)
	out := make([]rune, 0, len(in))
	for _, r := range in {
		if m, ok := composeTable[r]; ok && len(out) > 0 {
			if c, ok := m[out[len(out)-1]]; ok {
				out[len(out)-1] = c
				continue
			}
		}
		out = append(out, r)
	}
	return string(out)
}

var composeTable = map[rune]map[rune]rune{}

func init() {
	add := func(mark rune, pairs string) {
		m := map[rune]rune{}
		f := strings.Fields(pairs)
		for _, p := range f {
			rs := []rune(p)
			m[rs[0]] = rs[1]
		}
		composeTable[mark] = m
	}
	add(0x300, "AÀ EÈ IÌ OÒ UÙ aà eè iì oò uù")
	add(0x301, "AÁ EÉ IÍ OÓ UÚ YÝ aá eé ií oó uú yý CĆ cć NŃ nń SŚ sś ZŹ zź")
	add(0x302, "AÂ EÊ IÎ OÔ UÛ aâ eê iî oô uû")
	add(0x303, "AÃ NÑ OÕ aã nñ oõ")
	add(0x308, "AÄ EË IÏ OÖ UÜ aä eë iï oö uü yÿ")
	add(0x30A, "AÅ aå UŮ uů")
	add(0x327, "CÇ cç SŞ sş")
	add(0x30C, "CČ cč SŠ sš ZŽ zž EĚ eě RŘ rř")
}
