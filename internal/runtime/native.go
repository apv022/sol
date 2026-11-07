package runtime

import (
	"context"
	"os/exec"
)

// Native runs programs found on PATH (e.g. installed with apt or brew).
type Native struct{}

func (n *Native) Name() string { return "native" }

func (n *Native) Available(program string) bool {
	_, err := exec.LookPath(program)
	return err == nil
}

func (n *Native) Start(ctx context.Context, p Program) (*Proc, error) {
	path, err := exec.LookPath(p.Name)
	if err != nil {
		return nil, err
	}
	return startBinary(ctx, path, p)
}
