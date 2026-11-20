// Command notes-dev runs the Notes app against an in-memory Sol so a person can try the UI in a
// browser without a daemon: a temporary synced folder seeded with realistic notes, and the memsol
// fake exposed at /_sol/ on the same origin as the UI, exactly where the real daemon's bridge is.
//
//	go run ./apps/notes/cmd/notes-dev
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/apv022/sol/apps/notes"
	"github.com/apv022/sol/sdk/go/sol"
	"github.com/apv022/sol/sdk/go/sol/memsol"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8788", "address for the UI")
	keep := flag.Bool("keep", false, "keep the temporary folder on exit")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, *addr, *keep); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, addr string, keep bool) error {
	tmp, err := os.MkdirTemp("", "notes-dev-")
	if err != nil {
		return err
	}
	if !keep {
		defer os.RemoveAll(tmp)
	}
	root := filepath.Join(tmp, "SolDrive")
	if err := Seed(root); err != nil {
		return err
	}
	net0 := memsol.New()
	alice := net0.AddPerson("Alice")
	dev := alice.Devices[0]
	app := notes.New(dev.Client("notes"), notes.Config{Root: root, DataDir: filepath.Join(tmp, "data"), Logf: log.Printf})
	m, err := sol.ParseManifest(notes.ManifestJSON)
	if err != nil {
		return err
	}
	alice.Install(m, app.Handler())
	if err := app.Start(ctx); err != nil {
		return err
	}
	defer app.Stop()

	c := dev.Client("notes")
	tok := c.Token()
	bridge := http.StripPrefix("/_sol", memsol.NewServer(net0))
	front := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/_sol/v1/") {
			r.Header.Set("Authorization", "Bearer "+tok)
			bridge.ServeHTTP(w, r)
			return
		}
		r.Header.Set("Sol-Person", alice.ID)
		r.Header.Set("Sol-Device", dev.ID)
		r.Header.Set("Sol-Self", "1")
		app.Handler().ServeHTTP(w, r)
	})
	srv := &http.Server{Addr: addr, Handler: front}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	fmt.Printf("Notes dev UI is running. Open:\n  http://%s/\n\nNotes folder on disk: %s\nEdit a .md file there with any editor and watch the UI follow.\nCtrl-C to stop.\n", addr, app.Dir())
	if err := srv.ListenAndServe(); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}
