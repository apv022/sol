package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// solBin builds the real binary once per test.
func solBin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "sol")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

func solRun(t *testing.T, bin, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "SOL_DIR="+dir, "HOME="+t.TempDir())
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("sol %v: %v\n%s", args, err, out.String())
		}
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("sol %v hung\n%s", args, out.String())
	}
	return out.String()
}

func TestBackgroundLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := solBin(t)
	dir := filepath.Join(t.TempDir(), "d")
	// A short path keeps unix sockets valid; the daemon falls back to a temp run dir anyway.
	solRun(t, bin, dir, "init", "--name", "Test", "--device", "t", "--ui", freeAddr(t), "--listen", "127.0.0.1:0")
	t.Cleanup(func() { _ = exec.Command(bin, "stop", "--dir", dir).Run() })

	out := solRun(t, bin, dir, "start")
	if !strings.Contains(out, "running in the background") || !strings.Contains(out, "http://") || !strings.Contains(out, "sol open") {
		t.Fatalf("start: %s", out)
	}
	// Single instance: a second start reports the running one, and a second `run` refuses.
	if out := solRun(t, bin, dir); !strings.Contains(out, "already running") || !strings.Contains(out, dir) {
		t.Fatalf("second start: %s", out)
	}
	cmd := exec.Command(bin, "run", "--dir", dir)
	if b, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(b), "already running") {
		t.Fatalf("second run: %v %s", err, b)
	}
	if out := solRun(t, bin, dir, "status"); !strings.Contains(out, "Test") {
		t.Fatalf("status: %s", out)
	}
	// logs -n: the log holds the daemon's startup output.
	if out := solRun(t, bin, dir, "logs", "-n", "1"); strings.Count(out, "\n") != 1 {
		t.Fatalf("logs -n 1: %q", out)
	}
	solRun(t, bin, dir, "restart")
	if out := solRun(t, bin, dir, "status"); !strings.Contains(out, "Test") {
		t.Fatalf("after restart: %s", out)
	}
	if out := solRun(t, bin, dir, "stop"); !strings.Contains(out, "stopped") {
		t.Fatalf("stop: %s", out)
	}
	if out := solRun(t, bin, dir, "stop"); !strings.Contains(out, "not running") {
		t.Fatalf("stop again: %s", out)
	}
	// sol open starts the daemon first.
	if out := solRun(t, bin, dir, "open", "--print"); !strings.Contains(out, "/login") {
		t.Fatalf("open: %s", out)
	}
	solRun(t, bin, dir, "stop")
}

// freeAddr returns a loopback address that was free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func TestTailAndRotate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sol.log")
	if err := os.WriteFile(p, []byte("a\nb\nc\nd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := tail(p, 2); got != "c\nd\n" {
		t.Fatalf("tail %q", got)
	}
	if got := tail(p, 10); got != "a\nb\nc\nd\n" {
		t.Fatalf("tail %q", got)
	}
	if err := os.WriteFile(p, make([]byte, logMaxBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	rotate(p)
	if _, err := os.Stat(p + ".1"); err != nil {
		t.Fatalf("not rotated: %v", err)
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("old log still in place")
	}
}

func TestFollowLog(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sol.log")
	if err := os.WriteFile(p, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pr, pw, _ := os.Pipe()
	done := make(chan struct{})
	go func() { _ = followLog(ctx, p, pw); close(done) }()
	time.Sleep(100 * time.Millisecond) // let the follower reach EOF
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString("new\n")
	f.Close()
	buf := make([]byte, 16)
	n, _ := pr.Read(buf)
	if string(buf[:n]) != "new\n" {
		t.Fatalf("got %q", buf[:n])
	}
	cancel()
	<-done
}

// TestFirstRun: plain `sol` with no data directory sets Sol up from the OS account, starts it
// and prints a one-time login URL (no browser: SSH_CONNECTION is set); it also adds the app-menu
// entry under HOME.
func TestFirstRun(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := solBin(t)
	home := t.TempDir()
	dir := filepath.Join(t.TempDir(), "d")
	run := func(args ...string) string {
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "SOL_DIR="+dir, "HOME="+home, "XDG_DATA_HOME=", "SSH_CONNECTION=1 2 3 4", "USER=tester")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("sol %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	t.Cleanup(func() { _ = exec.Command(bin, "stop", "--dir", dir).Run() })
	out := run()
	for _, w := range []string{"Welcome to Sol", dir, "http://localhost:", "/login?code=", "sol stop", "sol open"} {
		if !strings.Contains(out, w) {
			t.Fatalf("first run output lacks %q:\n%s", w, out)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "share", "applications", "sol.desktop")); err != nil && runtime.GOOS == "linux" {
		t.Fatalf("no launcher: %v", err)
	}
	if out := run("status"); !strings.Contains(out, "device") {
		t.Fatalf("status: %s", out)
	}
	if runtime.GOOS == "linux" {
		if out := run("launcher", "off"); !strings.Contains(out, "no longer") {
			t.Fatalf("launcher off: %s", out)
		}
	}
}
