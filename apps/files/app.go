// Package files is the Files app: one folder on disk that is the same on all of a person's
// devices, with subfolders shared with other people (design/apps/FILES.md).
//
// It is an ordinary Sol app: it talks to the daemon only through sdk/go/sol (feeds for the
// tree, blobs for content, shares for sharing, actions for everything a person or an agent can
// do) and serves its own UI. Nothing here knows about keys, transports or the daemon's storage.
//
// Invariants:
//   - the tree is the fold of feed entries (tree.go); the disk is a cache of it. A remote change
//     never overwrites a local edit that has not been put in the feed yet, and a conflicting edit
//     is never dropped: both survive, the loser as a "conflict copy";
//   - the disk index (what we last synced per path) is what separates "the user changed this"
//     from "the feed changed this", and is how our own writes are not echoed back;
//   - every path is validated for Windows-safety before it reaches the feed or the disk;
//   - every action requires a Sol-Self caller: peers reach only what is shared as feeds.
package files

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

const appID = "files"

// ManifestJSON is the app's app.json.
//
//go:embed app.json
var ManifestJSON []byte

// Device roles decide the keep-on-device defaults.
const (
	RoleDesktop = "desktop"
	RoleAndroid = "android"
)

// Config is how a Files instance is set up.
type Config struct {
	// Root is the synced folder. Empty means $SOL_FILES, then ~/SolDrive.
	Root string
	// DataDir holds Files' own state (index, keep choices). Empty means $SOL_APP_DATA.
	DataDir string
	// Role is RoleDesktop (default) or RoleAndroid.
	Role string
	// Listener is where the UI and actions are served; nil means sol.Serve (SOL_APP_LISTEN).
	Listener net.Listener
	// Debounce batches editor saves per path (default 300 ms).
	Debounce time.Duration
	// ScanEvery is the full-scan period on systems without a file watcher (default 10 s).
	ScanEvery time.Duration
	// RetryEvery is how soon work that failed because the daemon or a peer was unreachable is
	// tried again (default 2 s).
	RetryEvery time.Duration
	// NoWatch forces scan mode even where a watcher exists (tests).
	NoWatch bool
	// Now supplies the clock for new edits made through actions (tests).
	Now func() time.Time
	// Logf receives diagnostics; nil discards them. Never given secrets or file contents.
	Logf func(format string, args ...any)
}

// App is one running Files instance (one device).
type App struct {
	api      sol.API
	cfg      Config
	manifest *sol.Manifest
	root     string
	data     string

	me      string // my person id
	devID   string
	devName string

	// Lock order: syncMu, then pullMu, then mu. API calls are made without mu held.
	syncMu sync.Mutex // serialises everything that writes the disk or the feeds
	pullMu sync.Mutex // serialises feed reads so each entry is folded once
	mu     sync.RWMutex

	own        *Tree
	shares     map[string]*shareRec // my shares by id
	myShareID  map[string]bool      // every share id I ever created (their feeds are my mirrors)
	recv       map[string]*recvShare
	sharedBack map[string]bool // received write shares whose co-author feed I shared back to the owner
	cursors    map[string]uint64
	dirty      map[string]bool // feed addresses to read again
	petnames   map[string]string
	devStats   map[string]*devStat
	rev        uint64
	view       *view

	index      map[string]idxEntry
	keep       map[string]bool
	dirsMade   map[string]bool
	noticed    map[string]bool
	mergeTried map[string]bool // conflicts already given a merge attempt (key: conflictKey)
	skipped    map[string]string
	pending    map[string]bool
	pullErr    bool
	lastErr    string
	busy       int
	stats      Stats
	saveT      *time.Timer

	ign ignorer

	started  bool
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	wake     chan struct{}
	fsq      chan string
	watch    watcher
	watching bool
	handler  http.Handler
}

// Stats counts feed appends, for tests and the status page.
type Stats struct {
	TreeAppends  int
	ShareAppends int
	BlobUploads  int
	BlobFetches  int
}

type devStat struct {
	Entries int
	Last    time.Time
}

// New creates a Files instance for api. Call Start (or Run) to begin syncing.
func New(api sol.API, cfg Config) *App {
	m, err := sol.ParseManifest(ManifestJSON)
	if err != nil {
		panic("files: bad embedded app.json: " + err.Error()) // a build error, not a runtime one
	}
	if cfg.Role == "" {
		cfg.Role = RoleDesktop
	}
	if cfg.Debounce <= 0 {
		cfg.Debounce = 300 * time.Millisecond
	}
	if cfg.ScanEvery <= 0 {
		cfg.ScanEvery = 10 * time.Second
	}
	if cfg.RetryEvery <= 0 {
		cfg.RetryEvery = 2 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
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
	a := &App{api: api, cfg: cfg, manifest: m, root: filepath.Clean(cfg.Root), data: cfg.DataDir,
		own: NewTree(), shares: map[string]*shareRec{}, myShareID: map[string]bool{}, recv: map[string]*recvShare{}, sharedBack: map[string]bool{},
		cursors: map[string]uint64{}, dirty: map[string]bool{}, petnames: map[string]string{},
		devStats: map[string]*devStat{}, index: map[string]idxEntry{}, keep: map[string]bool{}, dirsMade: map[string]bool{},
		noticed: map[string]bool{}, mergeTried: map[string]bool{}, skipped: map[string]string{}, pending: map[string]bool{},
		wake: make(chan struct{}, 1), fsq: make(chan string, 256)}
	a.handler = a.buildHandler()
	return a
}

// Handler serves the UI, the raw file endpoint and the action endpoints.
func (a *App) Handler() http.Handler { return a.handler }

// Root returns the synced folder.
func (a *App) Root() string { return a.root }

// Stats returns a copy of the counters.
func (a *App) Stats() Stats {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.stats
}

func (a *App) logf(format string, args ...any) {
	if a.cfg.Logf != nil {
		a.cfg.Logf(format, args...)
	}
}

// Run starts syncing and serves until ctx ends.
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

// Start loads the state, subscribes to events, folds every feed, reconciles the folder with
// the tree and starts the watcher. It returns once the first pass is done.
func (a *App) Start(ctx context.Context) error {
	if a.started {
		return errors.New("files: already started")
	}
	if a.root == "" || a.root == "." {
		return errors.New("files: no folder: set SOL_FILES")
	}
	if err := os.MkdirAll(a.root, 0o755); err != nil {
		return fmt.Errorf("files: folder: %w", err)
	}
	if a.data == "" {
		return errors.New("files: no data directory: set SOL_APP_DATA")
	}
	if err := os.MkdirAll(a.data, 0o700); err != nil {
		return fmt.Errorf("files: data directory: %w", err)
	}
	w, err := a.api.WhoAmI(ctx)
	if err != nil {
		return fmt.Errorf("files: whoami: %w", err)
	}
	a.me, a.devID, a.devName = w.Person.ID, w.Device.ID, w.Device.Name
	a.loadState()
	a.ctx, a.cancel = context.WithCancel(ctx)
	a.started = true

	// Subscribe before reading, so an entry that lands in between is not missed (the cursor
	// per feed makes a duplicate harmless).
	evs, err := a.api.Events(a.ctx, sol.EventOptions{People: true})
	if err != nil {
		a.cancel()
		return fmt.Errorf("files: events: %w", err)
	}
	a.refreshPeople(a.ctx)
	if err := a.initialSync(a.ctx); err != nil {
		a.logf("files: initial sync: %v", err)
		a.noteErr(err)
	}
	a.startWatcher()
	a.wg.Add(1)
	go a.loop(evs)
	return nil
}

// Stop ends syncing and saves state.
func (a *App) Stop() {
	if !a.started {
		return
	}
	a.cancel()
	if a.watch != nil {
		_ = a.watch.Close()
	}
	a.wg.Wait()
	a.mu.Lock()
	if a.saveT != nil {
		a.saveT.Stop()
	}
	a.mu.Unlock()
	a.saveState()
	a.started = false
}
