package record

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/apv022/sol/internal/identity"
)

func signed(t *testing.T) (*identity.Identity, []byte) {
	t.Helper()
	key, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	data, err := Sign(key, "blog", map[string]string{"tor": "abc.onion", "i2p": "xyz.b32.i2p"})
	if err != nil {
		t.Fatal(err)
	}
	return key, data
}

func TestSignVerify(t *testing.T) {
	key, data := signed(t)
	rec, err := Verify(data)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Name != "blog" || rec.PubKey != identity.EncodePublicKey(key.Public()) {
		t.Fatalf("record: %+v", rec)
	}
	if !rec.Reaches("abc.onion") || !rec.Reaches("xyz.b32.i2p") || rec.Reaches("evil.onion") {
		t.Fatalf("Reaches is wrong for %+v", rec.Addrs)
	}

	// Reformatting the JSON must not break the signature.
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, data, "", "  "); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(pretty.Bytes()); err != nil {
		t.Fatalf("indented record: %v", err)
	}
}

func TestVerifyRejects(t *testing.T) {
	_, data := signed(t)
	other, _ := identity.Generate()
	cases := map[string][]byte{
		"changed address": bytes.Replace(data, []byte("abc.onion"), []byte("evl.onion"), 1),
		"changed name":    bytes.Replace(data, []byte(`"blog"`), []byte(`"shop"`), 1),
		"swapped key": bytes.Replace(data, []byte(mustField(t, data, "pubkey")),
			[]byte(identity.EncodePublicKey(other.Public())), 1),
		"future version": bytes.Replace(data, []byte(`"v":1`), []byte(`"v":2`), 1),
		"not json":       []byte("<html>"),
		"empty":          nil,
		"too large":      []byte(`{"record":{},"sig":"` + strings.Repeat("A", MaxSize) + `"}`),
	}
	for name, bad := range cases {
		if bytes.Equal(bad, data) {
			t.Fatalf("%s: the test did not change the record", name)
		}
		if _, err := Verify(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func mustField(t *testing.T, data []byte, field string) string {
	t.Helper()
	var env struct {
		Record map[string]any `json:"record"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatal(err)
	}
	s, _ := env.Record[field].(string)
	if s == "" {
		t.Fatalf("no %s in the record", field)
	}
	return s
}

func TestSignNeedsAnAddress(t *testing.T) {
	key, _ := identity.Generate()
	if _, err := Sign(key, "blog", nil); err == nil {
		t.Fatal("signed a record with no addresses")
	}
}

func TestHandler(t *testing.T) {
	_, data := signed(t)
	var have []byte
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "app "+r.URL.Path) })
	srv := httptest.NewServer(Handler(app, func() []byte { return have }))
	defer srv.Close()
	get := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if code, body := get("/page"); code != 200 || body != "app /page" {
		t.Fatalf("other paths must reach the app: %d %q", code, body)
	}
	// No record yet: not found, and the app never sees the request.
	if code, body := get(Path); code != 404 || strings.Contains(body, "app") {
		t.Fatalf("no record: %d %q", code, body)
	}
	have = data
	code, body := get(Path)
	if code != 200 {
		t.Fatalf("record: %d", code)
	}
	if _, err := Verify([]byte(body)); err != nil {
		t.Fatalf("served record: %v", err)
	}
}
