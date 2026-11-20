// Command browsertest runs Sol's browser JavaScript test suites in a real headless browser,
// with no Node.js. It serves the repository root, opens each suite's test/index.html in
// headless Firefox (one at a time, fresh profile each), and waits for the page to POST its
// result to /__result (ui/kit/testing.js does this). It exits non-zero if any suite fails or
// doesn't report in time.
//
//	go run ./cmd/browsertest              # every */test/index.html under the repo
//	go run ./cmd/browsertest sdk/js/test  # selected suites
//
// The browser is $SOL_BROWSER, else ~/.local/opt/firefox/firefox, else firefox on PATH.
package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "browsertest:", err)
		os.Exit(1)
	}
}

func browser() (string, error) {
	if b := os.Getenv("SOL_BROWSER"); b != "" {
		return b, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".local/opt/firefox/firefox")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return exec.LookPath("firefox")
}

func suites(args []string) ([]string, error) {
	if len(args) > 0 {
		return args, nil
	}
	var out []string
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "bin") {
			return filepath.SkipDir
		}
		if !d.IsDir() && d.Name() == "index.html" && filepath.Base(filepath.Dir(p)) == "test" {
			out = append(out, filepath.Dir(p))
		}
		return nil
	})
	return out, err
}

func run(args []string) error {
	bin, err := browser()
	if err != nil {
		return fmt.Errorf("no browser found (set SOL_BROWSER): %w", err)
	}
	list, err := suites(args)
	if err != nil {
		return err
	}
	results := make(chan string, 1)
	mux := http.NewServeMux()
	files := http.FileServer(http.Dir("."))
	// The style policy of every UI origin (internal/apphost.CSP), so a suite that renders an
	// inline style attribute fails (testing.js reports securitypolicyviolation). Scripts are
	// not restricted: the suite pages bootstrap with an inline module.
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "style-src 'self'")
		files.ServeHTTP(w, r)
	}))
	mux.HandleFunc("POST /__result", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		select {
		case results <- string(b):
		default:
		}
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	failed := 0
	for _, s := range list {
		res, err := runSuite(bin, ln.Addr().String(), filepath.ToSlash(s), results)
		switch {
		case err != nil:
			failed++
			fmt.Printf("FAIL %s: %v\n", s, err)
		case strings.HasPrefix(res, "PASS"):
			fmt.Printf("%s  %s\n", firstLine(res), s)
		default:
			failed++
			fmt.Printf("%s  %s\n%s\n", firstLine(res), s, indent(res))
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d suites failed", failed, len(list))
	}
	return nil
}

func runSuite(bin, addr, suite string, results chan string) (string, error) {
	profile, err := os.MkdirTemp("", "browsertest-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(profile)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	url := "http://" + addr + "/" + suite + "/index.html"
	cmd := exec.CommandContext(ctx, bin, "--headless", "--no-remote", "--profile", profile, url)
	cmd.Env = append(os.Environ(), "MOZ_HEADLESS=1")
	if err := cmd.Start(); err != nil {
		return "", err
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	select {
	case r := <-results:
		return r, nil
	case <-ctx.Done():
		return "", fmt.Errorf("no result within 60s")
	}
}

func firstLine(s string) string { l, _, _ := strings.Cut(s, "\n"); return l }

func indent(s string) string {
	_, rest, _ := strings.Cut(s, "\n")
	return "    " + strings.ReplaceAll(rest, "\n", "\n    ")
}
