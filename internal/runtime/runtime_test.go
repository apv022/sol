package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"testing/fstest"
)

func withBundle(t *testing.T, files fstest.MapFS) {
	t.Helper()
	old := bundledFS
	bundledFS = files
	t.Cleanup(func() { bundledFS = old })
}

func TestExtractBundledVerifiesChecksum(t *testing.T) {
	bin := []byte("#!/bin/sh\necho fake tor\n")
	sum := sha256.Sum256(bin)
	key := platformPath("tor")

	withBundle(t, fstest.MapFS{
		"bin/" + key:         {Data: bin},
		"bin/checksums.json": {Data: []byte(`{"` + key + `":"` + hex.EncodeToString(sum[:]) + `"}`)},
	})
	b := &Bundled{BinDir: t.TempDir()}
	if !b.Available("tor") || b.Available("i2pd") {
		t.Fatal("Available wrong")
	}
	path, err := ExtractBundled("tor", b.BinDir)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(bin) {
		t.Fatal("extracted content differs")
	}

	withBundle(t, fstest.MapFS{
		"bin/" + key:         {Data: []byte("tampered")},
		"bin/checksums.json": {Data: []byte(`{"` + key + `":"` + hex.EncodeToString(sum[:]) + `"}`)},
	})
	if _, err := ExtractBundled("tor", t.TempDir()); err == nil {
		t.Fatal("tampered binary was extracted")
	}

	withBundle(t, fstest.MapFS{"bin/" + key: {Data: bin}, "bin/checksums.json": {Data: []byte(`{}`)}})
	if _, err := ExtractBundled("tor", t.TempDir()); err == nil {
		t.Fatal("unpinned binary was extracted")
	}
}

func TestSelectFallsThrough(t *testing.T) {
	withBundle(t, nil)
	if _, err := Select([]string{"bundled", "docker"}, "definitely-not-a-program", Options{}); err == nil {
		t.Fatal("Select found a provider for a missing program")
	}
	if _, err := Select([]string{"nope"}, "tor", Options{}); err == nil {
		t.Fatal("unknown provider accepted")
	}
}

// SOL_BIN_<NAME> names a program's file directly (Android has no PATH for
// app binaries); a path that isn't a file is an error, not a silent
// fall-back to PATH.
func TestNativeEnvOverride(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // nothing on PATH
	n := &Native{}
	if n.Available("snowflake-proxy") {
		t.Fatal("found a program on an empty PATH")
	}
	bin := filepath.Join(t.TempDir(), "libsnow.so")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOL_BIN_SNOWFLAKE_PROXY", bin)
	if got, err := nativePath("snowflake-proxy"); err != nil || got != bin {
		t.Fatalf("nativePath = %q, %v", got, err)
	}
	t.Setenv("SOL_BIN_SNOWFLAKE_PROXY", filepath.Join(t.TempDir(), "missing"))
	if n.Available("snowflake-proxy") {
		t.Fatal("a missing SOL_BIN file counted as available")
	}
}

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// fakeBundle is a bundle with a tor script and a library next to it; tamper changes the
// library after pinning, unpinned leaves it out of checksums.json.
func fakeBundle(t *testing.T, script string, tamper, unpinned bool) {
	t.Helper()
	tor, lib := []byte(script), []byte("not really a library")
	sums := map[string]string{platformPath("tor"): sum(tor)}
	if !unpinned {
		sums[platform()+"/libevent-2.1.so.7"] = sum(lib)
	}
	if tamper {
		lib = []byte("a library someone swapped")
	}
	raw, _ := json.Marshal(sums)
	withBundle(t, fstest.MapFS{
		"bin/" + platformPath("tor"):               {Data: tor, Mode: 0o755},
		"bin/" + platform() + "/libevent-2.1.so.7": {Data: lib},
		"bin/checksums.json":                       {Data: raw},
	})
}

// Every file next to tor is extracted only if pinned and matching: a swapped or unpinned
// library refuses the whole bundle.
func TestBundledRefusesUnpinnedLibrary(t *testing.T) {
	fakeBundle(t, "#!/bin/sh\n", false, false)
	dir := t.TempDir()
	exe, err := ExtractBundled("tor", dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(exe), "libevent-2.1.so.7")); err != nil {
		t.Fatal("the library was not extracted next to tor:", err)
	}
	fakeBundle(t, "#!/bin/sh\n", true, false)
	if _, err := ExtractBundled("tor", t.TempDir()); err == nil || !strings.Contains(err.Error(), "pinned checksum") {
		t.Fatalf("a tampered library: %v", err)
	}
	fakeBundle(t, "#!/bin/sh\n", false, true)
	if _, err := ExtractBundled("tor", t.TempDir()); err == nil || !strings.Contains(err.Error(), "no pinned checksum") {
		t.Fatalf("an unpinned library: %v", err)
	}
}

// The bundled tor runs with its own folder on the library path.
func TestBundledLibraryPath(t *testing.T) {
	if goruntime.GOOS == "windows" {
		if env := LibraryEnv(`C:\x`); env != nil {
			t.Fatalf("Windows needs no library path, got %v", env)
		}
		t.Skip("Windows finds DLLs next to the .exe; no shell script to run")
	}
	out := filepath.Join(t.TempDir(), "env")
	fakeBundle(t, "#!/bin/sh\necho \"$LD_LIBRARY_PATH|$DYLD_LIBRARY_PATH\" > "+out+"\n", false, false)
	t.Setenv("LD_LIBRARY_PATH", "/old")
	t.Setenv("DYLD_LIBRARY_PATH", "/old")
	b := &Bundled{BinDir: t.TempDir()}
	if !b.Available("tor") {
		t.Fatal("not available")
	}
	p, err := b.Start(context.Background(), Program{Name: "tor", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	<-p.Done()
	got, _ := os.ReadFile(out)
	want := filepath.Join(b.BinDir, platform()) + string(os.PathListSeparator) + "/old"
	ld, dyld, _ := strings.Cut(strings.TrimSpace(string(got)), "|")
	if goruntime.GOOS == "darwin" {
		ld, dyld = dyld, ld
	}
	if ld != want || dyld != "/old" {
		t.Fatalf("library path %q (other %q), want %q", ld, dyld, want)
	}
}

// A bundle that can't be used (here: a tampered library) falls back to the system's tor.
func TestBundledFallsBackToNative(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("needs a shell script as the native tor")
	}
	fakeBundle(t, "#!/bin/sh\n", true, false)
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	if err := os.WriteFile(filepath.Join(bin, "tor"), []byte("#!/bin/sh\n: > "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	p, err := Select([]string{"bundled", "native"}, "tor", Options{BinDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "bundled" {
		t.Fatalf("selected %s, want bundled (it is embedded)", p.Name())
	}
	proc, err := p.Start(context.Background(), Program{Name: "tor", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	<-proc.Done()
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the native tor did not run:", err)
	}
	// Without a native tor, the refusal is the error.
	t.Setenv("PATH", t.TempDir())
	p, _ = Select([]string{"bundled", "native"}, "tor", Options{BinDir: t.TempDir()})
	if _, err := p.Start(context.Background(), Program{Name: "tor", DataDir: t.TempDir()}); err == nil {
		t.Fatal("a tampered bundle ran")
	}
}

// With -tags bundled and the bundle fetched (scripts/fetch-tor.sh), the embedded tor
// extracts, passes its checksums and runs with its own libraries.
func TestRealBundledTor(t *testing.T) {
	b := &Bundled{BinDir: t.TempDir()}
	if !b.Available("tor") {
		t.Skip("no bundled tor in this build (go test -tags bundled after scripts/fetch-tor.sh)")
	}
	exe, err := ExtractBundled("tor", b.BinDir)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "--version")
	cmd.Env = append(os.Environ(), LibraryEnv(filepath.Dir(exe))...)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "Tor version 0.4.9.16") {
		t.Fatalf("%s: %v", out, err)
	}
}

// The platform's tor wins over a bundled and a native one: SOL_TOR_BIN first, then the
// config's tor_bin. It runs without a checksum check, its version is logged, and a named file
// that is missing is an error instead of a quiet fall-back.
func TestPlatformTorPriority(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("uses shell scripts as tor")
	}
	script := func(dir, name, version string) string {
		p := filepath.Join(dir, name)
		body := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo '" + version + "'; exit 0; fi\n: > \"$0.ran\"\n"
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	dir := t.TempDir()
	fromEnv := script(dir, "libtor.so", "Tor version 0.4.9.16 (android).")
	fromCfg := script(dir, "cfg-tor", "Tor version 0.4.9.15.")
	pathDir := t.TempDir()
	script(pathDir, "tor", "Tor version 0.0.0 (native).")
	t.Setenv("PATH", pathDir)
	fakeBundle(t, "#!/bin/sh\n", false, false) // a usable bundle, which must still lose

	run := func(paths map[string]string) string {
		t.Helper()
		var logged strings.Builder
		log := slog.New(slog.NewTextHandler(&logged, nil))
		p, err := Select(TorProviders, "tor", Options{BinDir: t.TempDir(), Platform: paths, Log: log})
		if err != nil {
			t.Fatal(err)
		}
		proc, err := p.Start(context.Background(), Program{Name: "tor", DataDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		<-proc.Done()
		return p.Name() + "|" + logged.String()
	}

	t.Setenv(TorBinEnv, fromEnv)
	if got := run(PlatformPaths(fromCfg)); !strings.HasPrefix(got, "platform|") || !strings.Contains(got, "0.4.9.16 (android)") {
		t.Fatalf("SOL_TOR_BIN did not win: %s", got)
	}
	if _, err := os.Stat(fromEnv + ".ran"); err != nil {
		t.Fatal("the SOL_TOR_BIN tor did not run")
	}
	t.Setenv(TorBinEnv, "")
	if got := run(PlatformPaths(fromCfg)); !strings.HasPrefix(got, "platform|") || !strings.Contains(got, "0.4.9.15") {
		t.Fatalf("tor_bin did not win: %s", got)
	}
	if got := run(PlatformPaths("")); !strings.HasPrefix(got, "bundled|") {
		t.Fatalf("without a platform tor: %s", got)
	}
	t.Setenv(TorBinEnv, filepath.Join(dir, "missing"))
	if _, err := Select(TorProviders, "tor", Options{BinDir: t.TempDir(), Platform: PlatformPaths("")}); err == nil || !strings.Contains(err.Error(), "not a program file") {
		t.Fatalf("a missing SOL_TOR_BIN: %v", err)
	}
}
