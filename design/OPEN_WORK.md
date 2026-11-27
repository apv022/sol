# Open work (handoff, 2026-11-26)

Where things stood when work paused. Released: v0.2.0-beta.4. On `main` since then (unreleased):
app channels v1, Bluetooth LE, the Files rename-race fix, the agent design draft.

## CI

- **Red: `TestFallbackAndReturn` (internal/session/paths_test.go), about 1 run in 80 locally,
  more on CI.** Two symptoms: "timed out waiting for a session over tor" (no Tor session within
  10 s after direct is refused) and "path states: []" (the Tor session reports "best path" but
  the side that dialed it has no failure record for direct). An earlier change only relaxed the
  reason check; that is not a fix. Suspects: the session draining change (053c5a2: a replaced
  session drains, `Retired()`, the keeper waits on it) changing which session is current or
  delaying the redial; hedged dialing; the tie-break. Instrumentation from the investigation is
  on branch `wip/fallback-flake` (not for main). Find the root cause, fix, prove with
  `-race -count=100`.

## Features left from the plan

- **Channels UI:** "Get more apps" and Updates on the Apps page; Settings › Apps shows each app's
  channel, version, policy and Rollback (actions and admin endpoints exist).
- **Android APK updates through channels:** the shell downloads the APK, checks its sha256 against
  the release and its package and signing certificate against the app's binding, installs with
  PackageInstaller (REQUEST_INSTALL_PACKAGES; no prompt on Android 12+ for apps Sol installed),
  and reports the installed release to the daemon (new admin endpoint, so the pinned seq moves).
- **Channels follow-ups:** blob relay between my devices, garbage collection of stored artifacts,
  a "channel moved" record for a leaked key.
- **Wi-Fi Direct and BLE:** built, never run on real phones. Needs two Android 10+ phones: discovery,
  group join without a dialog, sessions over the p2p link, BLE adverts and L2CAP, battery cost.
- **Virtual network (Tailscale-like):** after reliability, per APV.
- **Agent:** design draft in design/apps/AGENT.md with four open questions for APV (name/repo, web
  tools in v1, default key scope, voice). Build only after APV answers.
- **Music sharing app:** APV wants it next after the agent; not designed yet.
- **Profile:** the app API has no avatar field yet (note in API.md).

## Raypass

- A stable release key exists at `~/.config/raypass-release/` (back it up). It still has to be
  stored as the repo secrets RAYPASS_KEYSTORE_B64, RAYPASS_KEY_ALIAS (`raypass-release`) and
  RAYPASS_KEYSTORE_PASS on apv022/raypass (blocked for the agent; APV runs the `gh secret set`
  commands). After the first release with it: uninstall the old Raypass once, then in Sol remove
  Raypass under Settings › Connected apps and connect it again (its signing key changed).

## Housekeeping

- `apv022/sol-mobile` is generated from `android/` (git subtree split, fast-forward only): push it
  before each release. It was last updated at 65080db (2026-11-26).
