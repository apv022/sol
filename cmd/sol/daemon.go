package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/apv022/sol/internal/control"
)

// This file is the background-node part of the CLI: `sol` (or `sol start`)
// starts the node as a detached process and returns, `sol stop` and
// `sol restart` talk to it over the control socket, and `sol logs` shows
// what it wrote. `sol run` (main.go) is the node itself, in the foreground;
// it is what the background process, systemd and launchd run.

const (
	logName     = "sol.log"
	logMaxBytes = 5 << 20 // sol.log is moved to sol.log.1 when a node starts with it this large
	startWait   = 20 * time.Second
	stopWait    = 15 * time.Second
)

func logPath(data string) string { return filepath.Join(data, logName) }

// spawnNode starts `sol run` for data as a detached process writing to the
// log file. It returns a channel that is closed when that process exits.
func spawnNode(data, panel string) (<-chan struct{}, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(data, 0o700); err != nil {
		return nil, err
	}
	path := logPath(data)
	if fi, err := os.Stat(path); err == nil && fi.Size() > logMaxBytes {
		os.Rename(path, path+".1") // keep one old log; a failure only means a longer file
	}
	logf, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer logf.Close() // the child has its own copy
	args := []string{"run", "--data", data}
	if panel != "" {
		args = append(args, "--panel", panel)
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting the node: %w", err)
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	return exited, nil
}

// nodeStatus asks the running node who it is; ErrNotRunning if none answers.
func nodeStatus(ctx context.Context, data string) (control.Status, error) {
	var st control.Status
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	err := control.Call(cctx, sock(data), control.Request{Op: control.OpStatus}, &st)
	return st, err
}

// waitUp waits for a node on data to answer, or for exited to close.
func waitUp(ctx context.Context, data string, exited <-chan struct{}) (control.Status, error) {
	deadline := time.After(startWait)
	for {
		if st, err := nodeStatus(ctx, data); err == nil {
			return st, nil
		}
		select {
		case <-exited:
			return control.Status{}, fmt.Errorf("the node stopped right after starting. Its last lines (%s):\n%s", logPath(data), tail(logPath(data), 8))
		case <-deadline:
			return control.Status{}, fmt.Errorf("the node didn't answer within %s; see %s", startWait, logPath(data))
		case <-ctx.Done():
			return control.Status{}, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// waitDown waits until no node answers on data.
func waitDown(ctx context.Context, data string) error {
	deadline := time.After(stopWait)
	for {
		if _, err := nodeStatus(ctx, data); errors.Is(err, control.ErrNotRunning) {
			return nil
		}
		select {
		case <-deadline:
			return fmt.Errorf("the node is still running after %s", stopWait)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func printRunning(st control.Status, data string, already bool) {
	if already {
		fmt.Printf("Sol is already running (node %q).\n", st.Name)
	} else {
		fmt.Printf("Sol is running in the background (node %q).\n", st.Name)
	}
	if st.Panel != "" {
		fmt.Printf("  Panel:   http://%s\n", st.Panel)
	}
	if len(st.Addrs) == 0 {
		fmt.Println("  Network: connecting; `sol status` shows when it's online")
	}
	for _, t := range sortedKeys(st.Addrs) {
		fmt.Printf("  %-8s %s\n", t+":", st.Addrs[t])
	}
	fmt.Println("  Stop it with `sol stop`, see what it's doing with `sol logs -f`.")
	if !already {
		fmt.Printf("  Data:    %s (back up node.key: it is this node's identity)\n", data)
	}
}

// runStart is `sol` and `sol start`: start the node in the background and
// give the terminal back.
func runStart(ctx context.Context, args []string) error {
	var panel string
	data, pos := flags("start", args, func(fs *flag.FlagSet) {
		fs.StringVar(&panel, "panel", "", "panel listen address (default 127.0.0.1:<panel_port>)")
	})
	if len(pos) > 0 {
		return fmt.Errorf("unknown argument %q; run `sol help`", pos[0])
	}
	if st, err := nodeStatus(ctx, data); err == nil {
		printRunning(st, data, true)
		return nil
	}
	exited, err := spawnNode(data, panel)
	if err != nil {
		return err
	}
	st, err := waitUp(ctx, data, exited)
	if err != nil {
		return err
	}
	printRunning(st, data, false)
	return nil
}

// runStop is `sol stop`.
func runStop(ctx context.Context, args []string) error {
	data, _ := flags("stop", args, nil)
	if err := control.Call(ctx, sock(data), control.Request{Op: control.OpStop}, nil); err != nil {
		if errors.Is(err, control.ErrNotRunning) {
			fmt.Println("Sol isn't running.")
			return nil
		}
		return err
	}
	if err := waitDown(ctx, data); err != nil {
		return err
	}
	fmt.Println("Sol stopped. Peers can't reach this machine until you start it again with `sol`.")
	return nil
}

// runRestart is `sol restart`: the node restarts itself (so a node kept by
// systemd or launchd stays under it), or is started if it wasn't running.
func runRestart(ctx context.Context, args []string) error {
	data, _ := flags("restart", args, nil)
	old, err := nodeStatus(ctx, data)
	if errors.Is(err, control.ErrNotRunning) {
		return runStart(ctx, args)
	}
	if err != nil {
		return err
	}
	if err := control.Call(ctx, sock(data), control.Request{Op: control.OpRestart}, nil); err != nil {
		return err
	}
	// The old node answers until it has shut down, and its replacement can
	// be up a moment later, so "no answer" may never be seen. Wait for a
	// node with another process ID instead.
	var st control.Status
	deadline := time.After(stopWait + startWait)
	for {
		if st, err = nodeStatus(ctx, data); err == nil && st.PID != old.PID {
			break
		}
		select {
		case <-deadline:
			return fmt.Errorf("the node didn't come back; see %s", logPath(data))
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	fmt.Println("Restarted.")
	printRunning(st, data, false)
	return nil
}

// runLogs is `sol logs [-f] [-n N]`.
func runLogs(ctx context.Context, args []string) error {
	var follow bool
	var n int
	data, _ := flags("logs", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&follow, "f", false, "keep printing new lines (Ctrl+C to stop)")
		fs.IntVar(&n, "n", 40, "how many of the last lines to print")
	})
	path := logPath(data)
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no log yet at %s (a node started with `sol` writes one; `sol run` prints to the terminal instead)", path)
	}
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Print(tail(path, n))
	if !follow {
		return nil
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	buf := make([]byte, 32<<10)
	for {
		nr, err := f.Read(buf)
		if nr > 0 {
			os.Stdout.Write(buf[:nr])
			continue
		}
		if err != nil && err != io.EOF {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// tail returns the last n lines of the file at path ("" if it can't be read).
func tail(path string, n int) string {
	f, err := os.Open(path)
	if err != nil || n <= 0 {
		return ""
	}
	defer f.Close()
	// Logs are small (rotated at logMaxBytes); the last 256 KiB hold far
	// more than any n a person asks for.
	if fi, err := f.Stat(); err == nil && fi.Size() > 256<<10 {
		f.Seek(-256<<10, io.SeekEnd)
	}
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
}

// runConfig is `sol config [key [value]]`: the settings the panel also shows.
func runConfig(ctx context.Context, args []string) error {
	data, pos := flags("config", args, nil)
	req := control.Request{Op: control.OpConfig}
	switch len(pos) {
	case 0, 1:
	case 2:
		req = control.Request{Op: control.OpConfigSet, Key: pos[0], Value: pos[1]}
	default:
		// `sol config stun cloudflare google` reads better than quoting a list.
		req = control.Request{Op: control.OpConfigSet, Key: pos[0], Value: strings.Join(pos[1:], ",")}
	}
	var cfg control.Config
	if err := control.Call(ctx, sock(data), req, &cfg); err != nil {
		return err
	}
	show := func(o control.Option) string {
		if o.Value == "" {
			return "(none)"
		}
		return o.Value
	}
	switch {
	case len(pos) == 0:
		for _, o := range cfg.Options {
			note := ""
			if o.Restart {
				note = "  [needs restart]"
			}
			fmt.Printf("%-17s %s%s\n    %s\n", o.Key, show(o), note, o.Help)
		}
	default:
		found := false
		for _, o := range cfg.Options {
			if o.Key == pos[0] {
				found = true
				fmt.Printf("%s = %s\n", o.Key, show(o))
				if len(pos) == 1 {
					fmt.Println(o.Help)
				}
			}
		}
		if !found {
			return fmt.Errorf("unknown setting %q; `sol config` lists them", pos[0])
		}
	}
	if cfg.RestartNeeded {
		fmt.Println("A changed setting takes effect after a restart: sol restart")
	}
	return nil
}
