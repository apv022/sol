// Package notes is the Notes app: Markdown notes that are plain .md files inside the synced
// folder (design/apps/FILES.md), under <SOL_FILES>/Notes/.
//
// Notes never syncs anything itself. Files carries the folder between devices and merges or
// conflicts concurrent edits; Notes only reads and writes the files on disk and keeps a search
// index of them in memory.
//
// Invariants:
//   - the files are the only source of truth. The index (titles, tags, links, a full-text
//     inverted index) is rebuilt from them at startup and kept fresh by a watcher, so a note
//     edited in vim or by another device shows up without any action;
//   - every path is validated (no "..", no hidden or Windows-invalid names) and resolved
//     through symlinks, so nothing outside Notes/ is ever read or written;
//   - every write is atomic (temp file in the same folder, then rename) and the temp name is
//     one Files ignores, so a half-written note is never synced;
//   - links are by name: [[Title]] finds a note by file name, then H1 title, case-insensitively.
//     Renaming a note rewrites those links in other notes (an option, on by default);
//   - every action requires a Sol-Self caller; the UI uses only actions.
package notes

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/apv022/sol/sdk/go/sol"
)

// ManifestJSON is the app's app.json.
//
//go:embed app.json
var ManifestJSON []byte

// Config is how a Notes instance is set up.
type Config struct {
	// Root is the synced folder. Empty means $SOL_FILES, then ~/SolDrive.
	Root string
	// DataDir holds Notes' own state (pins). Empty means $SOL_APP_DATA.
	DataDir string
	// Listener is where the UI and actions are served; nil means sol.Serve (SOL_APP_LISTEN).
	Listener net.Listener
	// Debounce batches bursts of file events (default 150 ms).
	Debounce time.Duration
	// ScanEvery is the full-scan period (default 10 s without a watcher, 60 s with one).
	ScanEvery time.Duration
	// NoWatch forces scan mode even where a watcher exists (tests).
	NoWatch bool
	// Logf receives diagnostics; nil discards them. Never given note text.
	Logf func(format string, args ...any)
}

// App is one running Notes instance.
type App struct {
	api      sol.API
	cfg      Config
	manifest *sol.Manifest
	root     string // the synced folder
	notes    string // root/Notes
	data     string

	wmu sync.Mutex // serialises everything that writes notes or the index from disk
	idx *index

	pinMu sync.Mutex
	pins  map[string]bool

	started  bool
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	watch    watcher
	wake     chan struct{}
	handler  http.Handler
	watching bool
}

// New creates a Notes instance. api may be nil when the app is only used through Handler (the
// app itself never calls the daemon: it uses the folder and actions only).
func New(api sol.API, cfg Config) *App {
	m, err := sol.ParseManifest(ManifestJSON)
	if err != nil {
		panic("notes: bad embedded app.json: " + err.Error())
	}
	if cfg.Debounce <= 0 {
		cfg.Debounce = 150 * time.Millisecond
	}
	if cfg.Root == "" {
		cfg.Root = os.Getenv("SOL_FILES")
	}
	if cfg.Root == "" {
		if h, err := os.UserHomeDir(); err == nil {
			cfg.Root = filepath.Join(h, "SolDrive")
		}
	}
	if cfg.DataDir == "" {
		cfg.DataDir = os.Getenv("SOL_APP_DATA")
	}
	root := filepath.Clean(cfg.Root)
	a := &App{api: api, cfg: cfg, manifest: m, root: root, notes: filepath.Join(root, NotesDir), data: cfg.DataDir,
		idx: newIndex(), pins: map[string]bool{}, wake: make(chan struct{}, 1)}
	a.handler = a.buildHandler()
	return a
}

// Handler serves the UI, attachments and the action endpoints.
func (a *App) Handler() http.Handler { return a.handler }

// Dir returns the Notes folder on disk.
func (a *App) Dir() string { return a.notes }

// Revision counts index changes; it grows whenever any note, folder or tag changes.
func (a *App) Revision() uint64 { return a.idx.revision() }

func (a *App) logf(format string, args ...any) {
	if a.cfg.Logf != nil {
		a.cfg.Logf(format, args...)
	}
}

// Run starts the app and serves until ctx ends.
func (a *App) Run(ctx context.Context) error {
	if err := a.Start(ctx); err != nil {
		return err
	}
	defer a.Stop()
	var err error
	if a.cfg.Listener != nil {
		err = sol.ServeListener(ctx, a.cfg.Listener, a.api, a.handler)
	} else {
		err = sol.Serve(ctx, a.api, a.handler)
	}
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// Start creates Notes/ if needed, builds the index from the files and starts the watcher.
func (a *App) Start(ctx context.Context) error {
	if a.started {
		return errors.New("notes: already started")
	}
	if a.root == "" || a.root == "." {
		return errors.New("notes: no folder: set SOL_FILES")
	}
	if err := os.MkdirAll(a.notes, 0o755); err != nil {
		return fmt.Errorf("notes: folder: %w", err)
	}
	if a.data != "" {
		if err := os.MkdirAll(a.data, 0o700); err != nil {
			return fmt.Errorf("notes: data directory: %w", err)
		}
	}
	a.loadPins()
	if err := a.fullScan(); err != nil {
		return fmt.Errorf("notes: index: %w", err)
	}
	ctx, a.cancel = context.WithCancel(ctx)
	a.started = true
	a.startWatcher()
	a.wg.Add(1)
	go a.loop(ctx)
	return nil
}

// Stop ends the watcher.
func (a *App) Stop() {
	if !a.started {
		return
	}
	a.cancel()
	if a.watch != nil {
		_ = a.watch.Close()
	}
	a.wg.Wait()
	a.started = false
}
