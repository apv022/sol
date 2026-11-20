package e2e

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sold "github.com/apv022/sol/internal/daemon"
)

// restoreNode restores alice's person from her words in a fresh directory, asking from (a link
// of a device that knows her), and starts it.
func restoreNode(t *testing.T, words []string, from string) *sold.Daemon {
	t.Helper()
	dir := t.TempDir()
	cfg := sold.Config{UI: "127.0.0.1:0", Files: filepath.Join(dir, "SolDrive"),
		Transports: []sold.TransportConfig{{Name: "clearnet", Listen: "127.0.0.1:0"}}}
	if err := sold.Restore(context.Background(), dir, sold.RestoreOptions{Words: strings.Join(words, " "), Device: "new", From: from, Config: cfg}); err != nil {
		t.Fatal(err)
	}
	d, err := sold.Start(sold.Options{Dir: dir, InProcessApps: true, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// TestRestoreContacts documents what a restore brings back. Contacts live in the replicated
// "sol/contacts" feed, which only my own devices hold (a contact never gets it), so a restore
// with no surviving device cannot learn them: the restored device starts with none and each
// contact is added again by pairing (their device still lists Alice, and re-pairing is one
// scan). Pairing with a contact adds them back, and the old contacts do not leak in from
// anywhere else.
func TestRestoreContacts(t *testing.T) {
	alice := startNode(t, sold.InitOptions{Name: "Alice", Device: "laptop"}, nil)
	bob := startNode(t, sold.InitOptions{Name: "Bob", Device: "desktop"}, nil)
	pairNodes(t, alice, bob)
	words, err := alice.BackupWords()
	if err != nil {
		t.Fatal(err)
	}
	waitSession(t, bob, alice)
	from, _ := bob.Invite(false)
	restored := restoreNode(t, words, from)
	time.Sleep(time.Second)
	if n := len(restored.People()); n != 0 {
		t.Fatalf("a restore with no surviving device learned %d contacts from nowhere", n)
	}
	// Re-pairing brings Bob back.
	link, _ := bob.Invite(false)
	if _, _, err := restored.Pair(context.Background(), link); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Bob is a contact again", func() bool { ps := restored.People(); return len(ps) == 1 && ps[0].Name == "Bob" })
}
