package daemon

import (
	"log/slog"
	"net/netip"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/apv022/sol/sdk/go/sol"
)

type fakeNotes struct {
	mu sync.Mutex
	ns []Notification
}

func (f *fakeNotes) post(n Notification) { f.mu.Lock(); f.ns = append(f.ns, n); f.mu.Unlock() }
func (f *fakeNotes) count() int {
	time.Sleep(50 * time.Millisecond) // the notifier runs in its own goroutine
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.ns)
}

func notifyDaemon(t *testing.T, f *fakeNotes) *Daemon {
	t.Helper()
	t.Setenv("SOL_NOTIFY", "")
	t.Setenv("ANDROID_ROOT", "")
	dir := t.TempDir()
	cfg := Config{UI: "127.0.0.1:0", Files: filepath.Join(dir, "files"), Transports: []TransportConfig{{Name: "clearnet", Listen: "127.0.0.1:0"}}}
	if err := Init(dir, InitOptions{Name: "A", Device: "d", Config: cfg}); err != nil {
		t.Fatal(err)
	}
	d, err := Start(Options{Dir: dir, InProcessApps: true, APIOverTCP: true, Log: slog.New(slog.DiscardHandler),
		SameUser: func(_, _ netip.AddrPort) (bool, error) { return false, nil }, Notifier: f.post})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestNotifyInbox(t *testing.T) {
	f := &fakeNotes{}
	d := notifyDaemon(t, f)
	item := func(id string) sol.InboxItem {
		return sol.InboxItem{ID: id, App: "calendar", Kind: "notice", Title: "Standup", Body: "in 5 min", Time: time.Now()}
	}
	d.notifyInbox(item("a"))
	d.notifyInbox(item("a")) // same item again
	if n := f.count(); n != 1 {
		t.Fatalf("new item: %d notifications, want 1", n)
	}
	if f.ns[0].Title != "Standup" || f.ns[0].Body != "in 5 min" || f.ns[0].App != "calendar" {
		t.Fatalf("notification %+v", f.ns[0])
	}
	ans := item("b")
	ans.Answer = "approved"
	old := item("c")
	old.Time = time.Now().Add(-48 * time.Hour)
	d.notifyInbox(ans)
	d.notifyInbox(old)
	long := item("d")
	long.Body = ""
	for i := 0; i < 1000; i++ {
		long.Body += "x"
	}
	d.notifyInbox(long)
	if n := f.count(); n != 2 {
		t.Fatalf("answered/old must not notify: %d, want 2", n)
	}
	if l := len([]rune(f.ns[1].Body)); l > bodyMax {
		t.Fatalf("body not capped: %d", l)
	}

	if _, err := d.SetNotifications(false); err != nil {
		t.Fatal(err)
	}
	d.notifyInbox(item("e"))
	if n := f.count(); n != 2 {
		t.Fatalf("off: %d, want 2", n)
	}
	if st := d.Notifications(); st.Desktop || st.Active {
		t.Fatalf("state %+v", st)
	}
	if _, err := d.SetNotifications(true); err != nil {
		t.Fatal(err)
	}
	d.notifyInbox(item("e"))
	if n := f.count(); n != 3 {
		t.Fatalf("on again: %d, want 3", n)
	}

	t.Setenv("SOL_NOTIFY", "off")
	d.notifyInbox(item("f"))
	if n := f.count(); n != 3 {
		t.Fatalf("SOL_NOTIFY=off: %d, want 3", n)
	}
}

func TestNotifyAndroid(t *testing.T) {
	f := &fakeNotes{}
	d := notifyDaemon(t, f)
	old := notifyGOOS
	notifyGOOS = "android"
	t.Cleanup(func() { notifyGOOS = old })
	d.notifyInbox(sol.InboxItem{ID: "x", Title: "T", Time: time.Now()})
	if n := f.count(); n != 0 {
		t.Fatalf("android notified %d", n)
	}
	if st := d.Notifications(); st.Supported || st.Active {
		t.Fatalf("android state %+v", st)
	}
	t.Setenv("SOL_NOTIFY", "on") // an explicit override wins
	d.notifyInbox(sol.InboxItem{ID: "y", Title: "T", Time: time.Now()})
	if n := f.count(); n != 1 {
		t.Fatalf("SOL_NOTIFY=on: %d", n)
	}
}
