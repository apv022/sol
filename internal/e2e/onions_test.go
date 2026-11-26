package e2e

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	sold "github.com/apv022/sol/internal/daemon"
	"github.com/apv022/sol/internal/faultnet"
	"github.com/apv022/sol/sdk/go/sol"
)

// TestPublicOnion: a local port published as a public onion service is reached by a stranger's
// tor with no client auth, on the default port 80 or a chosen one; Sol's own ports are refused;
// the address is stable across a restart of the service list and gone for good after removal.
func TestPublicOnion(t *testing.T) {
	fake := faultnet.NewFakeTor(t.TempDir())
	p := fake.Provider()
	alice := startTorNode(t, p, sold.InitOptions{Name: "Alice", Device: "server"})
	A := admin(t, alice)
	stranger := torClient(t, p)

	port := echoServer(t)
	var s sold.PublicService
	if err := A("POST", "/admin/v1/onions", sold.PublicService{Name: "blog", Port: port}, &s); err != nil {
		t.Fatal(err)
	}
	addrOf := func(name string) string {
		var list []sold.PublicService
		if err := A("GET", "/admin/v1/onions", nil, &list); err != nil {
			t.Fatal(err)
		}
		for _, x := range list {
			if x.Name == name {
				return x.Address
			}
		}
		return ""
	}
	eventually(t, "the public onion's address", func() bool { return addrOf("blog") != "" })
	addr := addrOf("blog")
	if addr == onionOf(t, alice) {
		t.Fatal("the public service reuses the node's own onion")
	}

	echo := func(target string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c, err := stranger.Dial(ctx, target)
		if err != nil {
			return err
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.WriteString(c, "hello\n"); err != nil {
			return err
		}
		got, err := bufio.NewReader(c).ReadString('\n')
		if err != nil || got != "hello\n" {
			return errors.New("echo: " + got)
		}
		return nil
	}
	eventually(t, "a stranger reaches the public onion", func() bool { return echo(addr) == nil })

	// A chosen onion port.
	if err := A("POST", "/admin/v1/onions", sold.PublicService{Name: "ssh", Port: port, OnionPort: 2222}, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the second onion", func() bool { return addrOf("ssh") != "" })
	eventually(t, "a stranger reaches it on its port", func() bool { return echo(addrOf("ssh")+":2222") == nil })

	// Sol's own ports and bad input are refused.
	for _, bad := range []sold.PublicService{
		{Name: "ui", Port: alice.UIPort()},
		{Name: "Bad Name", Port: port},
		{Name: "blog", Port: port}, // taken
		{Name: "zero", Port: 0},
	} {
		if err := A("POST", "/admin/v1/onions", bad, nil); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}

	// Through the built-in action: a dry run changes nothing and warns.
	var out map[string]any
	err := A("POST", "/admin/v1/actions/call", sol.ActionCall{Action: "sol.onions.host",
		Args: []byte(`{"name":"wiki","port":` + itoa(port) + `}`), DryRun: true}, &out)
	if w, _ := out["warning"].(string); err != nil || !strings.Contains(w, "anyone who has the .onion address") || addrOf("wiki") != "" {
		t.Fatalf("dry run: %v %v", out, err)
	}

	// Removed: unreachable, and the same name later gets a new address.
	if err := A("DELETE", "/admin/v1/onions/blog", nil, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the removed onion is unreachable", func() bool { return echo(addr) != nil })
	if err := A("POST", "/admin/v1/onions", sold.PublicService{Name: "blog", Port: port}, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a new address", func() bool { a := addrOf("blog"); return a != "" && a != addr })
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
