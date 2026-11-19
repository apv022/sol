package runtime

import "context"

// Docker runs programs in containers (docker or podman).
//
// It is a registered stub: Available always reports false, so Select skips
// it. To implement it:
//   - Available: `<Binary> image inspect <image>` succeeds.
//   - Start: WriteConfigFiles, then `<Binary> run --rm --network host
//     -v <DataDir>:/data <image> <Args...>` and wrap the process in a Proc
//     (startBinary on the container CLI does most of this).
//
// The Anonymous Public Mode egress lock (docs/spec.md 7.6) will also live
// here: a container on an internal-only network whose only route out is Tor.
type Docker struct {
	Binary string            // "docker" or "podman"
	Images map[string]string // program -> pinned image reference
}

func (d *Docker) Name() string { return "docker" }

func (d *Docker) Available(program string) bool { return false }

func (d *Docker) Start(ctx context.Context, p Program) (*Proc, error) {
	return nil, ErrNotImplemented
}
