package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/apv022/sol/internal/channels"
	"github.com/apv022/sol/internal/daemon"
	"github.com/apv022/sol/internal/identity"
)

const channelUsage = `usage:
  sol channel new <name> [--out FILE]      make a channel key (0600; default <dir>/channel-keys/<name>.key)
  sol channel publish --key NAME|FILE --app ID --version V [--notes TEXT] --artifact PLATFORM=FILE...
                      [--mirror PLATFORM=https://...]... [--out FILE]
                                           sign a release; without --out, host it on this node's
                                           public onion (Tor must be on) with the artifacts
  sol channel import <record-file> [--name NAME] [--blob FILE]...
                                           hand a record signed elsewhere (CI) to this node: hosted
                                           when it is this node's channel (or --name is given),
                                           else offered to the channel I follow
  sol channel list [--json]                the channels this node hosts, with their follow links
  sol channel remove <name>                stop hosting a channel; its .onion is gone for good

Following and installing are actions: sol do sol.apps.follow --link 'sol://channel?...',
sol do sol.apps.available, sol do sol.apps.install --id ID (sol do --help).`

// channelCmd is `sol channel ...`: making channel keys and publishing releases (design/CHANNELS.md).
// The key never leaves this process: records are signed here, and only the record and the
// artifacts go to the daemon.
func channelCmd(args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New(channelUsage)
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("sol channel "+sub, flag.ContinueOnError)
	dir := fs.String("dir", daemon.DefaultDir(), "data directory")
	asJSON := fs.Bool("json", false, "print JSON")
	out := fs.String("out", "", "new: where to write the key file; publish: write the signed record here instead of publishing")
	key := fs.String("key", "", "publish: the channel key (a name in <dir>/channel-keys, or a file)")
	app := fs.String("app", "", "publish: the app id (its app.json id)")
	version := fs.String("version", "", "publish: the version (its app.json version)")
	notes := fs.String("notes", "", "publish: release notes")
	name := fs.String("name", "", "import: the channel's name on this node when it is new here")
	var arts, mirrors, blobs list
	fs.Var(&arts, "artifact", "publish: PLATFORM=FILE (linux-amd64=app.tar.gz, android=app.apk); repeatable")
	fs.Var(&mirrors, "mirror", "publish: PLATFORM=https://... an HTTPS copy of that artifact; repeatable")
	fs.Var(&blobs, "blob", "import: an artifact file the record names; repeatable")
	if err := parseAnywhere(fs, rest); err != nil {
		return err
	}
	ctx := context.Background()
	switch sub {
	case "new":
		if fs.NArg() != 1 {
			return errors.New("usage: sol channel new <name> [--out FILE]")
		}
		k, err := channels.NewKey(fs.Arg(0))
		if err != nil {
			return err
		}
		p := *out
		if p == "" {
			p = channelKeyPath(*dir, k.Name)
		}
		p = absOrEmpty(p)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		if err := k.Save(p); err != nil {
			return err
		}
		fmt.Printf("Channel %s: %s\nKey file: %s (keep it safe: whoever has it can publish releases)\n", k.Name, k.ID(), p)
		return nil
	case "publish":
		return channelPublish(ctx, *dir, *key, *app, *version, *notes, arts, mirrors, *out, *asJSON)
	case "import":
		if fs.NArg() != 1 {
			return errors.New("usage: sol channel import <record-file> [--name NAME] [--blob FILE]...")
		}
		raw, err := os.ReadFile(fs.Arg(0))
		if err != nil {
			return err
		}
		r, err := identity.ParseRelease(raw)
		if err != nil {
			return err
		}
		c, err := daemon.Connect(*dir)
		if err != nil {
			return err
		}
		for _, b := range blobs {
			if _, err := uploadBlob(ctx, c, b); err != nil {
				return err
			}
		}
		var hosted []daemon.HostedInfo
		if err := c.Do(ctx, "GET", "/admin/v1/channels/hosted", nil, &hosted); err != nil {
			return err
		}
		ch := identity.ID(r.Channel)
		mine := *name != ""
		for _, h := range hosted {
			if h.Channel == ch {
				mine, *name = true, h.Name
			}
		}
		if mine {
			var hi daemon.HostedInfo
			if err := c.Do(ctx, "POST", "/admin/v1/channels/hosted", map[string]any{"name": *name, "record": raw}, &hi); err != nil {
				return err
			}
			return printHosted(hi, *asJSON)
		}
		if err := c.Do(ctx, "POST", "/admin/v1/channels/records", map[string]any{"record": raw}, nil); err != nil {
			return err
		}
		fmt.Printf("%s %s is now known to my devices.\n", r.App, r.Version)
		return nil
	case "list":
		c, err := daemon.Connect(*dir)
		if err != nil {
			return err
		}
		var hosted []daemon.HostedInfo
		if err := c.Do(ctx, "GET", "/admin/v1/channels/hosted", nil, &hosted); err != nil {
			return err
		}
		if *asJSON {
			return printJSON(hosted)
		}
		if len(hosted) == 0 {
			fmt.Println("This node hosts no channel. Make one: sol channel new <name>, then sol channel publish.")
		}
		for _, h := range hosted {
			if err := printHosted(h, false); err != nil {
				return err
			}
		}
		return nil
	case "remove":
		if fs.NArg() != 1 {
			return errors.New("usage: sol channel remove <name>")
		}
		c, err := daemon.Connect(*dir)
		if err != nil {
			return err
		}
		return c.Do(ctx, "DELETE", "/admin/v1/channels/hosted/"+fs.Arg(0), nil, nil)
	}
	return errors.New(channelUsage)
}

// channelKeyPath is where `sol channel new` keeps a channel key by default.
func channelKeyPath(dir, name string) string { return filepath.Join(dir, "channel-keys", name+".key") }

// loadChannelKey reads --key: a file path, or a name under <dir>/channel-keys.
func loadChannelKey(dir, key string) (channels.Key, error) {
	if key == "" {
		return channels.Key{}, errors.New("publish: --key is required")
	}
	if channels.NameRe.MatchString(key) {
		if _, err := os.Stat(key); err != nil {
			key = channelKeyPath(dir, key)
		}
	}
	return channels.LoadKey(key)
}

// hashFile returns a file's SHA-256 and size.
func hashFile(p string) ([]byte, uint64, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return nil, 0, err
	}
	if n == 0 || n > channels.MaxArtifactSize {
		return nil, 0, fmt.Errorf("%s: an artifact is 1 byte to %d MiB", p, channels.MaxArtifactSize>>20)
	}
	return h.Sum(nil), uint64(n), nil
}

func uploadBlob(ctx context.Context, c *daemon.Client, p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var res struct {
		SHA256 string `json:"sha256"`
	}
	if err := c.Upload(ctx, "/admin/v1/channels/blobs", f, &res); err != nil {
		return "", err
	}
	return res.SHA256, nil
}

// channelPublish signs a release and either writes it to out or hosts it on this node.
func channelPublish(ctx context.Context, dir, key, app, version, notes string, arts, mirrors []string, out string, asJSON bool) error {
	k, err := loadChannelKey(dir, key)
	if err != nil {
		return err
	}
	if app == "" || version == "" || len(arts) == 0 {
		return errors.New("publish: --app, --version and at least one --artifact are required")
	}
	r := identity.Release{App: app, Version: version, Notes: notes, Time: uint64(time.Now().Unix())}
	files := map[string]string{}
	for _, a := range arts {
		platform, file, ok := strings.Cut(a, "=")
		if !ok || platform == "" || file == "" {
			return fmt.Errorf("publish: --artifact %q must be PLATFORM=FILE", a)
		}
		sum, size, err := hashFile(file)
		if err != nil {
			return err
		}
		kind := identity.KindSolApp
		if platform == "android" || strings.HasSuffix(strings.ToLower(file), ".apk") {
			kind = identity.KindAPK
		}
		art := identity.Artifact{Platform: platform, Kind: kind, SHA256: sum, Size: size}
		for _, m := range mirrors {
			if p, u, ok := strings.Cut(m, "="); ok && p == platform {
				art.Mirrors = append(art.Mirrors, u)
			}
		}
		r.Artifacts = append(r.Artifacts, art)
		files[platform] = file
	}
	for _, m := range mirrors {
		if p, _, ok := strings.Cut(m, "="); !ok || files[p] == "" {
			return fmt.Errorf("publish: --mirror %q names no --artifact platform", m)
		}
	}
	if out != "" {
		// Offline (CI): the seq comes from the clock, so it still beats what was published.
		r.Seq = identity.NextReleaseSeq(0, time.Now())
		if err := r.Sign(k.Private()); err != nil {
			return err
		}
		if err := os.WriteFile(out, r.Marshal(), 0o644); err != nil {
			return err
		}
		fmt.Printf("Signed %s %s (seq %d) into %s. Hand it over with: sol channel import %s --blob FILE...\n", app, version, r.Seq, out, out)
		return nil
	}
	c, err := daemon.Connect(dir)
	if err != nil {
		return err
	}
	var hosted []daemon.HostedInfo
	if err := c.Do(ctx, "GET", "/admin/v1/channels/hosted", nil, &hosted); err != nil {
		return err
	}
	prev := uint64(0)
	for _, h := range hosted {
		for _, a := range h.Apps {
			if h.Channel == k.ID() && a.App == app {
				prev = a.Seq
			}
		}
	}
	r.Seq = identity.NextReleaseSeq(prev, time.Now())
	if err := r.Sign(k.Private()); err != nil {
		return err
	}
	for _, f := range files {
		if _, err := uploadBlob(ctx, c, f); err != nil {
			return err
		}
	}
	var hi daemon.HostedInfo
	if err := c.Do(ctx, "POST", "/admin/v1/channels/hosted", map[string]any{"name": k.Name, "record": r.Marshal()}, &hi); err != nil {
		return err
	}
	return printHosted(hi, asJSON)
}

func printHosted(h daemon.HostedInfo, asJSON bool) error {
	if asJSON {
		return printJSON(h)
	}
	fmt.Printf("Channel %s (%s)\n", h.Name, h.Channel)
	if h.Link != "" {
		fmt.Printf("  Follow link: %s\n", h.Link)
	} else {
		fmt.Println("  Follow link: once Tor has published the onion (sol channel list)")
	}
	for _, a := range h.Apps {
		blobs := ""
		if !a.Blobs {
			blobs = " (artifacts on mirrors only)"
		}
		fmt.Printf("  %s %s (seq %d)%s\n", a.App, a.Version, a.Seq, blobs)
	}
	return nil
}
