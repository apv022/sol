package snowflake

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/apv022/sol/internal/runtime"
)

// fakeProxy puts a shell script named snowflake-proxy first on PATH.
func fakeProxy(t *testing.T, script string) {
	t.Helper()
	if goruntime.GOOS == "windows" {
		t.Skip("the fake proxy is a shell script")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, Program), []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func native() (runtime.Provider, error) {
	p := &runtime.Native{}
	if !p.Available(Program) {
		return nil, errors.New("not installed")
	}
	return p, nil
}

func waitState(t *testing.T, h *Helper, want State) Status {
	t.Helper()
	for range 200 {
		if st := h.Status(); st.State == want {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("state = %+v, want %s", h.Status(), want)
	return Status{}
}

func TestOffByDefault(t *testing.T) {
	h := &Helper{DataDir: t.TempDir(), Provider: native}
	if st := h.Status(); st.Enabled || st.State != StateOff {
		t.Fatalf("a new helper must be off: %+v", st)
	}
	h.Stop() // stopping what never started is fine
}

func TestRunsAndStops(t *testing.T) {
	dir := t.TempDir()
	// Record the arguments, then stay up like the real proxy.
	fakeProxy(t, `echo "$@" > args; exec sleep 60`)
	h := &Helper{DataDir: dir, Provider: native}
	h.Start(context.Background())
	h.Start(context.Background()) // a second Start must not run a second proxy
	if st := waitState(t, h, StateRunning); !st.Enabled {
		t.Fatalf("running but not enabled: %+v", st)
	}
	var args string
	for range 200 {
		if b, err := os.ReadFile(filepath.Join(dir, "args")); err == nil && len(b) > 0 {
			args = string(b)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(args, "-capacity 10") || strings.Contains(args, "unsafe-logging") {
		t.Fatalf("proxy arguments: %q", args)
	}
	h.Stop()
	if st := h.Status(); st.Enabled || st.State != StateOff {
		t.Fatalf("after Stop: %+v", st)
	}
}

func TestMissingProgramIsReported(t *testing.T) {
	h := &Helper{DataDir: t.TempDir(), Provider: func() (runtime.Provider, error) { return nil, errors.New("not installed") }}
	h.Start(context.Background())
	defer h.Stop()
	st := waitState(t, h, StateMissing)
	for !strings.Contains(st.Detail, "isn't installed") { // "starting" comes first
		time.Sleep(10 * time.Millisecond)
		st = h.Status()
	}
	if !st.Enabled {
		t.Fatalf("missing must still count as turned on: %+v", st)
	}
}

func TestExitIsReportedAndRetried(t *testing.T) {
	old := restartMin
	restartMin = 300 * time.Millisecond
	defer func() { restartMin = old }()
	dir := t.TempDir()
	// Fail the first run, stay up on the second.
	fakeProxy(t, `if [ -e ran ]; then exec sleep 60; fi; touch ran; exit 3`)
	h := &Helper{DataDir: dir, Provider: native}
	h.Start(context.Background())
	defer h.Stop()
	if st := waitState(t, h, StateFailed); !strings.Contains(st.Detail, "Retrying") {
		t.Fatalf("failed detail: %+v", st)
	}
	waitState(t, h, StateRunning)
}
