// Command files-dev runs the Files app against an in-memory Sol network so a person can try the
// UI in a browser without a daemon: Alice with two devices (a laptop that serves the UI and a
// phone that keeps files online only) and Bob, whose own UI is served on a second port. The
// memsol fake is exposed over HTTP at /_sol/ on the same origin as each UI, exactly where the
// real daemon's bridge would be.
//
//	go run ./apps/files/cmd/files-dev
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/apv022/sol/apps/files"
	"github.com/apv022/sol/sdk/go/sol"
	"github.com/apv022/sol/sdk/go/sol/memsol"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8787", "address for Alice's UI (Bob's is the next port)")
	keep := flag.Bool("keep", false, "keep the temporary folders on exit")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, *addr, *keep); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, addr string, keep bool) error {
	tmp, err := os.MkdirTemp("", "files-dev-")
	if err != nil {
		return err
	}
	if !keep {
		defer os.RemoveAll(tmp)
	}
	net0 := memsol.New()
	alice, bob := net0.AddPerson("Alice"), net0.AddPerson("Bob")
	net0.Pair(alice, bob)
	laptop := alice.Devices[0]
	laptop.Name = "laptop"
	phone := alice.AddDevice("phone", false)

	mk := func(name string, dev *memsol.Device, role string) (*files.App, string) {
		root := filepath.Join(tmp, name, "SolDrive")
		app := files.New(dev.Client("files"), files.Config{Root: root, DataDir: filepath.Join(tmp, name, "data"), Role: role, Logf: log.Printf})
		return app, root
	}
	aliceApp, aliceRoot := mk("alice-laptop", laptop, files.RoleDesktop)
	phoneApp, _ := mk("alice-phone", phone, files.RoleAndroid)
	bobApp, _ := mk("bob", bob.Devices[0], files.RoleDesktop)
	if err := seed(aliceRoot); err != nil {
		return err
	}
	m, err := sol.ParseManifest(files.ManifestJSON)
	if err != nil {
		return err
	}
	alice.Install(m, aliceApp.Handler())
	bob.Install(m, bobApp.Handler())

	for _, a := range []*files.App{aliceApp, phoneApp, bobApp} {
		if err := a.Start(ctx); err != nil {
			return err
		}
		defer a.Stop()
	}
	// Share Photos with Bob so "Shared with me" has something in it.
	if _, err := laptop.Client("files").CallAction(ctx, sol.ActionCall{Action: "files.share",
		Args: []byte(`{"path":"Photos","people":["bob"],"mode":"read"}`)}); err != nil {
		return err
	}

	srv := memsol.NewServer(net0)
	urls := []string{}
	for i, u := range []struct {
		name string
		c    *memsol.Client
		app  *files.App
		p    *memsol.Person
	}{{"Alice (laptop)", laptop.Client("files"), aliceApp, alice}, {"Bob", bob.Devices[0].Client("files"), bobApp, bob}} {
		ln, err := listen(addr, i)
		if err != nil {
			return err
		}
		go func() { _ = http.Serve(ln, front(srv, u.c, u.app, u.p, laptop)) }()
		urls = append(urls, fmt.Sprintf("  %-15s http://%s/", u.name, ln.Addr()))
	}
	fmt.Printf("Files dev UI is running. Open:\n%s\n\nAlice's folder on disk: %s\nDrop files into it, or into the browser, and watch the phone and Bob follow.\nCtrl-C to stop.\n", strings.Join(urls, "\n"), aliceRoot)
	<-ctx.Done()
	return nil
}

func listen(addr string, i int) (net.Listener, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var p int
	fmt.Sscan(port, &p)
	if p != 0 {
		p += i
	}
	return net.Listen("tcp", net.JoinHostPort(host, fmt.Sprint(p)))
}

// front is one origin: /_sol/v1/ is the API bridge (the daemon adds the app token), everything
// else goes to the app with the caller headers the daemon would add for my own UI.
func front(api http.Handler, c *memsol.Client, app *files.App, p *memsol.Person, d *memsol.Device) http.Handler {
	tok := c.Token()
	bridge := http.StripPrefix("/_sol", api)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/_sol/v1/") {
			r.Header.Set("Authorization", "Bearer "+tok)
			bridge.ServeHTTP(w, r)
			return
		}
		r.Header.Set("Sol-Person", p.ID)
		r.Header.Set("Sol-Device", d.ID)
		r.Header.Set("Sol-Self", "1")
		app.Handler().ServeHTTP(w, r)
	})
}

func seed(root string) error {
	put := func(p string, b []byte) error {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		return os.WriteFile(full, b, 0o644)
	}
	if err := put("Documents/Welcome.md", []byte("# Welcome to Files\n\nThis folder is the same on all your devices.\nEdit this file in your editor or in the folder and watch it sync.\n")); err != nil {
		return err
	}
	if err := put("Notes/ideas.md", []byte("- build the thing\n- ship the thing\n")); err != nil {
		return err
	}
	if err := put("Notes/todo.txt", []byte("water the plants\n")); err != nil {
		return err
	}
	if err := put("Photos/gradient.png", gradient(color.RGBA{242, 165, 22, 255}, color.RGBA{40, 55, 107, 255})); err != nil {
		return err
	}
	if err := put("Photos/dusk.png", gradient(color.RGBA{179, 56, 47, 255}, color.RGBA{13, 18, 34, 255})); err != nil {
		return err
	}
	if err := put("Photos/sea.png", gradient(color.RGBA{86, 180, 211, 255}, color.RGBA{18, 64, 99, 255})); err != nil {
		return err
	}
	if err := put("Documents/Report.pdf", pdf("Quarterly report")); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(root, "Empty folder"), 0o755); err != nil {
		return err
	}
	return put("Music/tone.wav", tone(440, 2))
}

// pdf makes a one-page PDF with a title, so the PDF preview has something real to show.
func pdf(title string) []byte {
	content := fmt.Sprintf("BT /F1 28 Tf 72 720 Td (%s) Tj ET\nBT /F1 12 Tf 72 690 Td (Made by files-dev.) Tj ET", title)
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offs := make([]int, len(objs))
	for i, o := range objs {
		offs[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	x := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, o := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, x)
	return b.Bytes()
}

func gradient(a, b color.RGBA) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 320, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 320; x++ {
			t := float64(y) / 199
			img.Set(x, y, color.RGBA{uint8(float64(a.R)*(1-t) + float64(b.R)*t), uint8(float64(a.G)*(1-t) + float64(b.G)*t), uint8(float64(a.B)*(1-t) + float64(b.B)*t), 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// tone makes a small mono 16-bit WAV so the audio preview and Range seeking have something real.
func tone(hz float64, seconds int) []byte {
	const rate = 22050
	n := rate * seconds
	var buf bytes.Buffer
	w := func(v any) { _ = binary.Write(&buf, binary.LittleEndian, v) }
	buf.WriteString("RIFF")
	w(uint32(36 + n*2))
	buf.WriteString("WAVEfmt ")
	w(uint32(16))
	w(uint16(1))
	w(uint16(1))
	w(uint32(rate))
	w(uint32(rate * 2))
	w(uint16(2))
	w(uint16(16))
	buf.WriteString("data")
	w(uint32(n * 2))
	for i := 0; i < n; i++ {
		w(int16(9000 * math.Sin(2*math.Pi*hz*float64(i)/rate)))
	}
	return buf.Bytes()
}
