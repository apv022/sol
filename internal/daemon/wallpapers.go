package daemon

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/apv022/sol/internal/fsutil"
	"github.com/apv022/sol/sdk/go/sol"
)

// Custom wallpapers (design/ADMIN.md "Wallpapers"). The person's own images live in
// <files>/.sol/wallpapers/<id>.<ext> (png, jpeg or webp; written atomically, 0600 files in a
// 0700 folder). The id is random and the only name a request can carry: it is matched against
// wallpaperIDRe before it touches a path, so no id can leave the folder. The wallpaper setting
// names one as "custom:<id>"; the Sol app's UI root serves it at /wallpapers/<id>.

// MaxWallpaperBytes caps one uploaded wallpaper.
const MaxWallpaperBytes = 15 << 20

var wallpaperIDRe = regexp.MustCompile(`^[a-f0-9]{16}$`)

// wallpaperTypes maps the accepted media types to the file extension kept on disk.
var wallpaperTypes = map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/webp": "webp"}

// WallpaperInfo is one uploaded wallpaper (GET /admin/v1/wallpapers).
type WallpaperInfo struct {
	ID      string    `json:"id"`
	Type    string    `json:"type"`
	Size    int64     `json:"size"`
	Added   time.Time `json:"added"`
	URL     string    `json:"url"`
	Setting string    `json:"setting"` // the value of the wallpaper setting that picks it
}

func (d *Daemon) wallpaperDir() string { return filepath.Join(d.solDir(), "wallpapers") }

// sniffWallpaper checks the bytes really are the claimed image type (the header is the
// client's word; the magic bytes are ours).
func sniffWallpaper(b []byte, ctype string) bool {
	switch ctype {
	case "image/png":
		return bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n"))
	case "image/jpeg":
		return bytes.HasPrefix(b, []byte{0xff, 0xd8, 0xff})
	case "image/webp":
		return len(b) > 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP"
	}
	return false
}

func extType(ext string) string {
	switch ext {
	case ".png":
		return "image/png"
	case ".jpg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	}
	return ""
}

// AddWallpaper stores an uploaded image and returns it. ctype is the request's Content-Type.
func (d *Daemon) AddWallpaper(ctype string, r io.Reader) (WallpaperInfo, error) {
	if d.cfg.Files == "" {
		return WallpaperInfo{}, sol.Errorf("unavailable", "no synced folder configured")
	}
	mt, _, _ := mime.ParseMediaType(ctype)
	ext, ok := wallpaperTypes[mt]
	if !ok {
		return WallpaperInfo{}, sol.Errorf("bad_request", "a wallpaper is a png, jpeg or webp image")
	}
	b, err := io.ReadAll(io.LimitReader(r, MaxWallpaperBytes+1))
	if err != nil {
		return WallpaperInfo{}, sol.Errorf("bad_request", "could not read the image")
	}
	if len(b) > MaxWallpaperBytes {
		return WallpaperInfo{}, sol.Errorf("bad_request", "a wallpaper is at most 15 MB")
	}
	if !sniffWallpaper(b, mt) {
		return WallpaperInfo{}, sol.Errorf("bad_request", "the file is not a "+strings.TrimPrefix(mt, "image/")+" image")
	}
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	id := hex.EncodeToString(raw[:])
	if err := os.MkdirAll(d.wallpaperDir(), 0o700); err != nil {
		return WallpaperInfo{}, err
	}
	if err := fsutil.WriteFileAtomic(filepath.Join(d.wallpaperDir(), id+"."+ext), b, 0o600); err != nil {
		return WallpaperInfo{}, err
	}
	return WallpaperInfo{ID: id, Type: mt, Size: int64(len(b)), Added: time.Now().UTC(), URL: "/wallpapers/" + id, Setting: "custom:" + id}, nil
}

// Wallpapers lists the uploaded wallpapers, oldest first.
func (d *Daemon) Wallpapers() []WallpaperInfo {
	out := []WallpaperInfo{}
	if d.cfg.Files == "" {
		return out
	}
	ents, _ := os.ReadDir(d.wallpaperDir())
	for _, e := range ents {
		base := e.Name()
		ext := filepath.Ext(base)
		id := strings.TrimSuffix(base, ext)
		fi, err := e.Info()
		if err != nil || e.IsDir() || !wallpaperIDRe.MatchString(id) || extType(ext) == "" {
			continue
		}
		out = append(out, WallpaperInfo{ID: id, Type: extType(ext), Size: fi.Size(), Added: fi.ModTime().UTC(), URL: "/wallpapers/" + id, Setting: "custom:" + id})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Added.Before(out[j].Added) || out[i].Added.Equal(out[j].Added) && out[i].ID < out[j].ID
	})
	return out
}

// wallpaperFile finds the file of an uploaded wallpaper by id.
func (d *Daemon) wallpaperFile(id string) (path, ctype string, ok bool) {
	if d.cfg.Files == "" || !wallpaperIDRe.MatchString(id) {
		return "", "", false
	}
	for ext, t := range map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".webp": "image/webp"} {
		p := filepath.Join(d.wallpaperDir(), id+ext)
		if _, err := os.Stat(p); err == nil {
			return p, t, true
		}
	}
	return "", "", false
}

// DeleteWallpaper removes an uploaded wallpaper; when it was the chosen one the setting
// goes back to the default and a theme event tells open UIs.
func (d *Daemon) DeleteWallpaper(id string) error {
	p, _, ok := d.wallpaperFile(id)
	if !ok {
		return sol.Errorf("not_found", "no such wallpaper")
	}
	if err := os.Remove(p); err != nil {
		return err
	}
	if d.themeSettings()["wallpaper"] == "custom:"+id {
		_, err := d.UpdateThemeSettings(map[string]string{"wallpaper": ""})
		return err
	}
	return nil
}

// serveWallpaper serves GET /wallpapers/<id> on the UI root. The image is private, so it needs
// the session cookie like the admin API (the page's own request carries it). nosniff and the
// type taken from the file's extension keep a browser from treating it as anything else.
func (d *Daemon) serveWallpaper(w http.ResponseWriter, r *http.Request) {
	if !d.hasSession(r) {
		sol.WriteError(w, sol.Errorf("unauthorized", "This browser isn't signed in to Sol."))
		return
	}
	p, ctype, ok := d.wallpaperFile(r.PathValue("id"))
	if !ok {
		sol.WriteError(w, sol.Errorf("not_found", "no such wallpaper"))
		return
	}
	f, err := os.Open(p)
	if err != nil {
		sol.WriteError(w, sol.Errorf("not_found", "no such wallpaper"))
		return
	}
	defer f.Close()
	fi, _ := f.Stat()
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable") // the id never changes content
	http.ServeContent(w, r, "", fi.ModTime(), f)
}
