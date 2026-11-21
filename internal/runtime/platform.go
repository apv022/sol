package runtime

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// TorBinEnv names the environment variable a platform sets to the tor it ships (Android: the
// APK's lib/<abi>/libtor.so). It wins over the config's tor_bin and every other provider.
const TorBinEnv = "SOL_TOR_BIN"

// Platform runs a program file the platform provides: one the OS package or app bundle
// already verified (an Android APK is signed as a whole), so no checksum is checked here.
// Select puts it first when Options.Platform names a file for the program. Its version
// (`<file> --version`, first line) is logged once, so a log says which tor ran.
type Platform struct {
	// Paths maps a program name ("tor") to its file.
	Paths map[string]string
	// Log receives the version line (nil: slog's default).
	Log *slog.Logger

	once sync.Once
}

// Name implements Provider.
func (p *Platform) Name() string { return "platform" }

// Available implements Provider: true when a file is named for program.
func (p *Platform) Available(program string) bool {
	path := p.Paths[program]
	if path == "" {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// Start implements Provider.
func (p *Platform) Start(ctx context.Context, prog Program) (*Proc, error) {
	path := p.Paths[prog.Name]
	if !p.Available(prog.Name) {
		return nil, fmt.Errorf("the platform's %s (%s) is not a program file", prog.Name, path)
	}
	p.once.Do(func() {
		log := p.Log
		if log == nil {
			log = slog.Default()
		}
		log.Info("using the platform's program", "program", prog.Name, "path", path, "version", Version(ctx, path))
	})
	return startBinary(ctx, path, prog)
}

// Version runs `<path> --version` (5 seconds at most) and returns the first line of its
// output, or a short reason it could not.
func Version(ctx context.Context, path string, env ...string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.Output()
	line, _, _ := strings.Cut(string(bytes.TrimSpace(out)), "\n")
	if line == "" && err != nil {
		return "unknown (" + err.Error() + ")"
	}
	return strings.TrimSpace(line)
}

// PlatformPaths returns the platform-provided program files: tor from SOL_TOR_BIN, else
// torBin (the config's tor_bin, set with `sol init --tor-bin`). Empty means none.
func PlatformPaths(torBin string) map[string]string {
	if v := os.Getenv(TorBinEnv); v != "" {
		torBin = v
	}
	if torBin == "" {
		return nil
	}
	return map[string]string{"tor": torBin}
}
