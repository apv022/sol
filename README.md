# Sol

One p2p layer that apps are built on, and a suite of slim, agent-native apps built on it:
Files (a synced folder, like Nextcloud without the server), Notes, Tasks, and more to come.
Your devices and the people you add talk to each other directly (WireGuard, TLS, or Tor),
with no account and no server in the middle.

## Install

### Linux and macOS

While Sol is in private testing, the installer lives in the private `apv022/sol-test` repo,
so sign in to GitHub first (`gh auth login`) and run:

```sh
gh api repos/apv022/sol-test/contents/install.sh -H 'Accept: application/vnd.github.raw' | sh
```

(Once the repo is public this becomes a plain `curl -fsSL <url>/install.sh | sh`.)

This downloads the `sol` binary for your machine (Tor is built in), checks its SHA-256, puts it
in `~/.local/bin`, adds that folder to your `PATH` (in `~/.profile`, `~/.bashrc`, `~/.zshrc`
and fish, where needed), and starts Sol. Open a new terminal afterwards and plain `sol` works.

The first `sol` sets everything up: it names you after your OS account, creates your synced
folder `~/SolDrive`, starts in the background and opens the Sol app in your browser, already
signed in. Sol also appears in your app menu.

Instead of `gh`, a `GITHUB_TOKEN` that can read the repo works too, or run `sh install.sh`
from a checkout. Options: `SOL_VERSION=<tag>`, `SOL_BIN_DIR=<dir>`, `SOL_NO_PATH=1`,
`SOL_NO_START=1` (see the top of [install.sh](install.sh)).

### Windows

Download `sol-windows-amd64.exe` from the releases page, rename it `sol.exe`, put it in a folder
on your `PATH`, and run `sol`.

### Android

Install `sol-android-arm64.apk` from the releases page (allow installs from your browser or file
manager when Android asks). Open Sol and choose:

- **Start new** if this is your first device,
- **Add to my Sol** to add the phone to the Sol you already have (on the other device: Devices,
  Add my device, and scan the QR code with the phone's camera),
- **Restore from backup words** if you lost your devices and have your 24 words.

Your synced folder shows up as "Sol" in the phone's Files app and in every app's file picker.

## Everyday commands

```sh
sol                 # start Sol in the background (sets it up on first run) and return
sol open            # open the Sol app in the browser
sol status          # this device, your people, connections
sol stop            # stop it;  sol restart, sol logs -f
sol autostart on    # start Sol when you log in
sol do --help       # every app action, for scripts and agents (--json, --dry-run)
```

## Your data and recovery

- **Files** live in `~/SolDrive` on every device (a normal folder: use it with any program) and
  sync between your devices. Files you share with a person sync to them too.
- **Back up your 24 words** (Settings, Backup). They are your identity: with them,
  `sol restore --words "..."` on a fresh machine (install with `SOL_NO_START=1` first, so the
  installer doesn't set up a new Sol), or "Restore from backup words" on Android, brings back who
  you are. Your files then sync back from any of your devices that is still around.
- The words restore your identity, not your files: if every device that held a file is gone,
  that file is gone. Keep one always-on device (an "anchor": a home server, a NAS, an old laptop)
  so there is always a copy.

## For developers

Read [AGENTS.md](AGENTS.md), then [design/VISION.md](design/VISION.md) (why) and
[design/ARCHITECTURE.md](design/ARCHITECTURE.md) (how).

```sh
make build          # bin/sol
make check          # gofmt + go vet + tests with -race
make browser-test   # browser suites in headless Firefox
android/build.sh    # the Android app (see android/README.md)
```
