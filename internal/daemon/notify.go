package daemon

// Desktop notifications for Inbox items. The daemon posts them itself through the operating
// system's own local notifier (notify-send, osascript, a PowerShell toast): never a push
// service, nothing leaves the device. Invariants: one notification per item per device (a
// bounded set of notified ids), never for an item that is already answered or old, never on
// Android (its shell posts its own) and never when switched off. A notification carries only
// the item's title and body (capped), never its action arguments or results.
//
// SOL_NOTIFY=off|on overrides the stored setting and the Android detection; the Android shell
// sets off.

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/internal/fsutil"
	"github.com/apv022/sol/sdk/go/sol"
)

// Notification is one desktop notification.
type Notification struct {
	Title, Body string
	// Icon is a PNG file path, or "" for none.
	Icon string
	// App is the id of the app the item came from.
	App string
	// Open brings the app up (signed in); the OS notifier calls it when the person clicks Open.
	Open func()
}

// Notifier posts a notification. Options.Notifier replaces the OS one (tests).
type Notifier func(Notification)

// notifyGOOS is the OS the notifier decides for; tests change it.
var notifyGOOS = runtime.GOOS

const (
	notifiedMax  = 512
	notifyMaxAge = 24 * time.Hour
	titleMax     = 100
	bodyMax      = 300
)

// NotifyState is what GET /admin/v1/notifications reports.
type NotifyState struct {
	// Desktop is whether desktop notifications are switched on (the stored setting).
	Desktop bool `json:"desktop"`
	// Active is whether notifications are actually posted now.
	Active bool `json:"active"`
	// Supported is false on Android and where no notifier exists; UIs hide the switch then.
	Supported bool `json:"supported"`
	// Override is "on" or "off" when SOL_NOTIFY forces the answer, else "".
	Override string `json:"override,omitempty"`
}

type notifyBook struct {
	mu   sync.Mutex
	seen map[string]bool
	ring []string
}

// notifyOverride reads SOL_NOTIFY: "on" or "off" wins over the setting and the Android check.
func notifyOverride() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SOL_NOTIFY"))) {
	case "off", "0", "false", "no":
		return "off"
	case "on", "1", "true", "yes":
		return "on"
	}
	return ""
}

func isAndroid() bool { return notifyGOOS == "android" || os.Getenv("ANDROID_ROOT") != "" }

func (d *Daemon) notifyFile() string { return filepath.Join(d.dir, "notify.json") }

// notifySetting is the stored switch; on by default.
func (d *Daemon) notifySetting() bool {
	b, err := os.ReadFile(d.notifyFile())
	if err != nil {
		return true
	}
	var s struct {
		Desktop *bool `json:"desktop"`
	}
	if json.Unmarshal(b, &s) != nil || s.Desktop == nil {
		return true
	}
	return *s.Desktop
}

// Notifications reports the desktop-notification state.
func (d *Daemon) Notifications() NotifyState {
	st := NotifyState{Desktop: d.notifySetting(), Override: notifyOverride()}
	have := d.opt.Notifier != nil || osNotifierAvailable()
	st.Supported = have && (!isAndroid() || st.Override == "on")
	st.Active = st.Supported && st.Override != "off" && (st.Desktop || st.Override == "on")
	return st
}

// SetNotifications switches desktop notifications on or off on this device.
func (d *Daemon) SetNotifications(on bool) (NotifyState, error) {
	b, _ := json.Marshal(map[string]bool{"desktop": on})
	if err := fsutil.WriteFileAtomic(d.notifyFile(), b, 0o600); err != nil {
		return NotifyState{}, err
	}
	return d.Notifications(), nil
}

func capRunes(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// notifyInbox posts a notification for an inbox item that is new on this device.
func (d *Daemon) notifyInbox(it sol.InboxItem) {
	if it.Answer != "" || it.ID == "" || (!it.Time.IsZero() && time.Since(it.Time) > notifyMaxAge) {
		return
	}
	if !d.Notifications().Active {
		return
	}
	d.notes.mu.Lock()
	if d.notes.seen == nil {
		d.notes.seen = map[string]bool{}
	}
	if d.notes.seen[it.ID] {
		d.notes.mu.Unlock()
		return
	}
	d.notes.seen[it.ID] = true
	d.notes.ring = append(d.notes.ring, it.ID)
	if len(d.notes.ring) > notifiedMax {
		delete(d.notes.seen, d.notes.ring[0])
		d.notes.ring = d.notes.ring[1:]
	}
	d.notes.mu.Unlock()

	n := Notification{Title: capRunes(it.Title, titleMax), Body: capRunes(it.Body, bodyMax), App: it.App}
	if n.Title == "" {
		n.Title = "Sol"
	}
	if n.Body == "" && it.Kind == "approval" {
		n.Body = "Needs your approval"
	}
	n.Icon = d.notifyIcon(it.App)
	n.Open = func() { d.openAppURL(it.App) }
	post := d.opt.Notifier
	if post == nil {
		post = osNotify(d.log)
	}
	go post(n)
}

// notifyIcon writes the app's 512 px icon under <dir>/notify-icons once and returns its path,
// or "" when the app has none.
func (d *Daemon) notifyIcon(app string) string {
	if d.Host == nil || app == "" {
		return ""
	}
	p := filepath.Join(d.dir, "notify-icons", app+".png")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	for _, a := range d.Host.MenuApps() {
		if a.ID == app && len(a.Icon) > 0 {
			if os.MkdirAll(filepath.Dir(p), 0o700) == nil && fsutil.WriteFileAtomic(p, a.Icon, 0o644) == nil {
				return p
			}
		}
	}
	return ""
}

// openAppURL opens the app's origin in the browser. The daemon signs a browser of this OS user
// in there on its own (the path `sol open <app>` takes), so no code is needed.
func (d *Daemon) openAppURL(app string) {
	for _, a := range d.Apps() {
		if a.ID == app && a.URL != "" {
			openURL(a.URL)
			return
		}
	}
}

// openURL starts the default browser on u; SOL_NO_BROWSER=1 never opens one.
func openURL(u string) {
	if os.Getenv("SOL_NO_BROWSER") == "1" {
		return
	}
	var cmd *exec.Cmd
	switch notifyGOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	if cmd.Start() == nil {
		go cmd.Wait()
	}
}

var (
	nsOnce    sync.Once
	nsActions bool
	// notifySlots bounds the goroutines waiting for a click.
	notifySlots = make(chan struct{}, 8)
)

// osNotifierAvailable reports whether this system has a notifier Sol may use. SOL_NOTIFY=off
// turns desktop notifications off; every test package that runs daemons sets it (testenv_test.go),
// since test daemons file inbox items by the dozen and must never pop up on the developer's desktop.
func osNotifierAvailable() bool {
	if os.Getenv("SOL_NOTIFY") == "off" {
		return false
	}
	tool := map[string]string{"linux": "notify-send", "darwin": "osascript", "windows": "powershell"}[notifyGOOS]
	if tool == "" {
		return false
	}
	_, err := exec.LookPath(tool)
	return err == nil
}

// osNotify is the notifier of this operating system. A missing tool is skipped with one debug
// log, never an error.
func osNotify(log *slog.Logger) Notifier {
	return func(n Notification) {
		if !osNotifierAvailable() {
			log.Debug("notifications: no notifier here; skipped", "os", notifyGOOS)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		var cmd *exec.Cmd
		switch notifyGOOS {
		case "linux":
			nsOnce.Do(func() {
				out, _ := exec.Command("notify-send", "--help").CombinedOutput()
				nsActions = strings.Contains(string(out), "--action")
			})
			args := []string{"--app-name=Sol"}
			if n.Icon != "" {
				args = append(args, "--icon="+n.Icon)
			}
			wait := nsActions && n.Open != nil
			if wait {
				select {
				case notifySlots <- struct{}{}:
					defer func() { <-notifySlots }()
				default:
					wait = false
				}
			}
			if wait {
				args = append(args, "--action=open=Open", "--wait")
			}
			out, err := exec.CommandContext(ctx, "notify-send", append(args, "--", n.Title, n.Body)...).Output()
			if err != nil {
				log.Debug("notifications: notify-send failed", "err", err)
			} else if wait && strings.TrimSpace(string(out)) == "open" {
				n.Open()
			}
			return
		case "darwin":
			cmd = exec.CommandContext(ctx, "osascript", "-e", "on run argv", "-e",
				"display notification (item 2 of argv) with title (item 1 of argv)", "-e", "end run", n.Title, n.Body)
		case "windows":
			// The text goes in through the environment, never into the script.
			cmd = exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", toastScript)
			cmd.Env = append(os.Environ(), "SOL_N_TITLE="+n.Title, "SOL_N_BODY="+n.Body)
		}
		if err := cmd.Run(); err != nil {
			log.Debug("notifications: failed", "err", err)
		}
	}
}

const toastScript = `[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] | Out-Null
$t = [Windows.UI.Notifications.ToastNotificationManager]::GetTemplateContent([Windows.UI.Notifications.ToastTemplateType]::ToastText02)
$n = $t.GetElementsByTagName('text')
$n.Item(0).AppendChild($t.CreateTextNode($env:SOL_N_TITLE)) | Out-Null
$n.Item(1).AppendChild($t.CreateTextNode($env:SOL_N_BODY)) | Out-Null
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('Sol').Show([Windows.UI.Notifications.ToastNotification]::new($t))`
