// Package builtins registers Sol's first-party apps (Files, Notes, Tasks, Calendar) with the app host, so the
// sol binary can run them as "builtin:<name>" apps. Each runner talks to the daemon only through
// the app API, exactly like a third-party app: it builds a sol.Client from the Env the host
// passes (never from the daemon's internals) and serves on Env.Listen.
package builtins

import (
	"context"
	"log"
	"os"
	"runtime"

	"github.com/apv022/sol/apps/calendar"
	"github.com/apv022/sol/apps/files"
	"github.com/apv022/sol/apps/notes"
	"github.com/apv022/sol/apps/tasks"
	"github.com/apv022/sol/internal/apphost"
	"github.com/apv022/sol/sdk/go/sol"
)

// All returns every builtin app by name.
func All() map[string]apphost.Builtin {
	return map[string]apphost.Builtin{
		"files":    {Manifest: files.ManifestJSON, Icons: files.Icons(), Run: runFiles},
		"notes":    {Manifest: notes.ManifestJSON, Icons: notes.Icons(), Run: runNotes},
		"tasks":    {Manifest: tasks.ManifestJSON, Icons: tasks.Icons(), Run: runTasks},
		"calendar": {Manifest: calendar.ManifestJSON, Icons: calendar.Icons(), Run: runCalendar},
	}
}

// logf is the apps' diagnostics sink: stderr, which the host forwards to its log.
var logf = log.New(os.Stderr, "", 0).Printf

func runFiles(ctx context.Context, env apphost.Env) error {
	api, err := sol.New(env.API, env.Token)
	if err != nil {
		return err
	}
	ln, err := apphost.Listen(env.Listen)
	if err != nil {
		return err
	}
	role := files.RoleDesktop
	if onAndroid() {
		role = files.RoleAndroid // folders default to online only: a phone doesn't mirror everything
	}
	return files.New(api, files.Config{Root: env.Files, DataDir: env.Data, Listener: ln, Logf: logf, Role: role}).Run(ctx)
}

func runNotes(ctx context.Context, env apphost.Env) error {
	api, err := sol.New(env.API, env.Token)
	if err != nil {
		return err
	}
	ln, err := apphost.Listen(env.Listen)
	if err != nil {
		return err
	}
	return notes.New(api, notes.Config{Root: env.Files, DataDir: env.Data, Listener: ln, Logf: logf}).Run(ctx)
}

func runTasks(ctx context.Context, env apphost.Env) error {
	api, err := sol.New(env.API, env.Token)
	if err != nil {
		return err
	}
	ln, err := apphost.Listen(env.Listen)
	if err != nil {
		return err
	}
	return tasks.New(api, tasks.Config{DataDir: env.Data, Listener: ln, Logf: logf}).Run(ctx)
}

func runCalendar(ctx context.Context, env apphost.Env) error {
	api, err := sol.New(env.API, env.Token)
	if err != nil {
		return err
	}
	ln, err := apphost.Listen(env.Listen)
	if err != nil {
		return err
	}
	return calendar.New(api, calendar.Config{DataDir: env.Data, Listener: ln, Logf: logf}).Run(ctx)
}

// onAndroid reports whether this sol runs inside the Android app (the node binary is built for
// linux or android; the Android runtime always sets ANDROID_ROOT).
func onAndroid() bool { return runtime.GOOS == "android" || os.Getenv("ANDROID_ROOT") != "" }
