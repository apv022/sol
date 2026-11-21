package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
)

// Native runs programs found on PATH (e.g. installed with apt or brew).
//
// A program's file can also be named directly with SOL_BIN_<NAME>
// (SOL_BIN_TOR=/path/to/tor), for platforms that have no PATH to speak of:
// an Android app may only run files from its native-library directory,
// where tor is called libtor.so.
//
// The first Start logs the file and its version.
type Native struct {
	Log *slog.Logger // nil: slog's default

	ver versionOnce
}

func (n *Native) Name() string { return "native" }

// nativePath finds program: SOL_BIN_<NAME> if set, PATH otherwise.
func nativePath(program string) (string, error) {
	env := "SOL_BIN_" + strings.ToUpper(strings.ReplaceAll(program, "-", "_"))
	if p := os.Getenv(env); p != "" {
		if fi, err := os.Stat(p); err != nil || fi.IsDir() {
			return "", fmt.Errorf("%s=%s is not a program file", env, p)
		}
		return p, nil
	}
	return exec.LookPath(program)
}

func (n *Native) Available(program string) bool {
	_, err := nativePath(program)
	return err == nil
}

func (n *Native) Start(ctx context.Context, p Program) (*Proc, error) {
	path, err := nativePath(p.Name)
	if err != nil {
		return nil, err
	}
	n.ver.log(ctx, n.Log, n.Name(), p.Name, path, nil)
	return startBinary(ctx, path, p)
}
