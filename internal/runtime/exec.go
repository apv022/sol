package runtime

import (
	"context"
	"fmt"
	"os"
)

// Exec runs one fixed program file with extra environment variables,
// whatever program name is asked for. It exists for tests and tools that
// stand in for tor (internal/faultnet runs a scripted fake tor this way);
// the node never selects it from config.
type Exec struct {
	// Path is the program file to run.
	Path string
	// Env is added to the child's environment ("KEY=value").
	Env []string
}

// Name implements Provider.
func (e *Exec) Name() string { return "exec" }

// Available implements Provider: true when Path is a file.
func (e *Exec) Available(string) bool {
	fi, err := os.Stat(e.Path)
	return err == nil && !fi.IsDir()
}

// Start implements Provider.
func (e *Exec) Start(ctx context.Context, p Program) (*Proc, error) {
	if !e.Available(p.Name) {
		return nil, fmt.Errorf("%s is not a program file", e.Path)
	}
	return startBinaryEnv(ctx, e.Path, p, e.Env)
}
