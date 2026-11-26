package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/apv022/sol/internal/blobnet"
	"github.com/apv022/sol/internal/builtins"
	sold "github.com/apv022/sol/internal/daemon"
	"github.com/apv022/sol/internal/identity"
)

// TestFilesPurgeFreesBlobEverywhere: "delete forever" in Files releases the blob (API.md §4), so
// both of my devices delete its bytes from disk and the contact the folder is shared with can no
// longer fetch it from either; a file still in the folder is untouched.
func TestFilesPurgeFreesBlobEverywhere(t *testing.T) {
	old := blobnet.ReleaseGrace
	blobnet.ReleaseGrace = 300 * time.Millisecond
	// Restored by a cleanup registered before the daemons start: cleanups run last-in first-out,
	// so this runs after every daemon is closed (a defer would run while they still read it).
	t.Cleanup(func() { blobnet.ReleaseGrace = old })

	bi := builtins.All()
	laptop := startNode(t, sold.InitOptions{Name: "Alice", Device: "laptop"}, bi)
	phone := joinNode(t, laptop, bi)
	bob := startNode(t, sold.InitOptions{Name: "Bob", Device: "desktop"}, bi)
	pairNodes(t, laptop, bob)
	waitSession(t, laptop, phone)
	waitSession(t, laptop, bob)
	eventually(t, "Files running everywhere", func() bool {
		for _, d := range []*sold.Daemon{laptop, phone, bob} {
			if code, _ := call(t, d, "files.list", `{}`); code != 200 {
				return false
			}
		}
		return true
	})
	hashOf := func(s string) string {
		d := sha256.Sum256([]byte(s))
		return hex.EncodeToString(d[:])
	}
	cat, dog := "a cat, purged forever", "a dog, kept"
	hc, hd := hashOf(cat), hashOf(dog)
	lroot, proot := filepath.Join(laptop.Dir(), "SolDrive"), filepath.Join(phone.Dir(), "SolDrive")
	if err := os.MkdirAll(filepath.Join(lroot, "Photos"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]string{"cat.txt": cat, "dog.txt": dog} {
		if err := os.WriteFile(filepath.Join(lroot, "Photos", name), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "both files on the phone", func() bool {
		b1, e1 := os.ReadFile(filepath.Join(proot, "Photos", "cat.txt"))
		b2, e2 := os.ReadFile(filepath.Join(proot, "Photos", "dog.txt"))
		return e1 == nil && e2 == nil && string(b1) == cat && string(b2) == dog
	})
	if code, out := call(t, laptop, "files.share", `{"path":"Photos","people":["bob"],"mode":"read"}`); code != 200 {
		t.Fatalf("share: %d %s", code, out)
	}
	bobID := identity.ID(bob.Keyring.Person())
	eventually(t, "Bob may fetch the cat from both devices", func() bool {
		return laptop.Refs.MayFetch(bobID, hc) && phone.Refs.MayFetch(bobID, hc)
	})
	for _, d := range []*sold.Daemon{laptop, phone} {
		if !d.Blobs.Has(hc) || !d.Blobs.Has(hd) {
			t.Fatal("a device lacks the blobs before the purge")
		}
	}

	// Delete the cat (to Trash), then delete it forever from the phone.
	code, out := call(t, laptop, "files.delete", `{"paths":["Photos/cat.txt"]}`)
	var del struct{ Trash []string }
	if code != 200 || json.Unmarshal([]byte(out), &del) != nil || len(del.Trash) != 1 {
		t.Fatalf("delete: %d %s", code, out)
	}
	if !laptop.Blobs.Has(hc) {
		t.Fatal("moving to Trash freed the bytes")
	}
	ids, _ := json.Marshal(del.Trash)
	eventually(t, "the deletion in the phone's Trash", func() bool {
		code, out := call(t, phone, "files.trash_list", `{}`)
		var l struct{ Items []any }
		return code == 200 && json.Unmarshal([]byte(out), &l) == nil && len(l.Items) == 1
	})
	if code, out := call(t, phone, "files.trash_purge", `{"ids":`+string(ids)+`}`); code != 200 {
		t.Fatalf("purge: %d %s", code, out)
	}
	eventually(t, "the cat's bytes gone from both devices", func() bool {
		return !laptop.Blobs.Has(hc) && !phone.Blobs.Has(hc)
	})
	for _, d := range []*sold.Daemon{laptop, phone} {
		if d.Refs.MayFetch(bobID, hc) {
			t.Fatal("Bob may still fetch the purged blob")
		}
		if !d.Blobs.Has(hd) || !d.Refs.MayFetch(bobID, hd) {
			t.Fatal("the file still in the folder was touched")
		}
	}
}
