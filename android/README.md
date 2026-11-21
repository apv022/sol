# Sol for Android

A thin Android shell around the same Go node (`sol`) that runs on a laptop. The APK ships the node as
`lib/<abi>/libsol.so` and Tor as `lib/<abi>/libtor.so`, a foreground service keeps the node running,
and one activity shows Sol's own UI in a WebView (`http://localhost:7777/`, apps at
`http://<app>.localhost:7777/`). There is no Gradle: the Java is built with `javac` + `d8`, packed
with `aapt2` and signed with `apksigner`.

## Build

You need Go, bash, zip, gpg, curl and a toolchain directory with JDK 17 and the Android SDK
(build-tools 35.0.0, android-35). The default is `~/sol-mobile/.toolchain`; point `TC=/path`
elsewhere to use your own.

```sh
android/build.sh                  # build/sol.apk: arm64-v8a (phones) + x86_64 (emulator), debug key
ABIS=arm64-v8a android/build.sh   # phones only
ABIS=x86_64 android/build.sh      # emulator only (a static linux/amd64 node)
SOL_SRC=/path/to/copy android/build.sh   # build the node from another checkout of this repo
```

The only download is Tor: the Tor Project's `tor-expert-bundle-android-<arch>-16.0.2.tar.gz` from
dist.torproject.org, fetched once per ABI into `$TC/dl`. Every build checks it again: the bundle's
own signature and the signed `sha256sums-signed-build.txt` must both be by the Tor Browser signing
key `EF6E286DDA85EA2A4BA7DE684E2C6E8793298290` (gpg runs in a throwaway `GNUPGHOME`), and the
bundle's hash must match the list. Any mismatch stops the build.

### Release build

```sh
export SOL_KEYSTORE=~/.config/sol-release/sol-release.p12   # PKCS12 or JKS
export SOL_KEY_ALIAS=sol-release
export SOL_KEYSTORE_PASS="$(cat ~/.config/sol-release/keystore.pass)"  # env only, never argv
SOL_VERSION=0.1.0 RELEASE=1 android/build.sh                # -> build/sol-release.apk
```

`RELEASE=1` builds arm64-v8a only, writes `android:debuggable="false"` into the manifest and signs
with your key (the password reaches `apksigner` through the environment, so it never shows in `ps`
or a log). It stops before building if any of the three variables is missing or the keystore does
not exist. Check the result with `apksigner verify --print-certs build/sol-release.apk`.

The debug build signs with a throwaway key kept in the toolchain directory. Android only upgrades
an app in place when the new APK has the same signing key, so a phone runs either debug builds or
release builds; switching means uninstalling first, which deletes the phone's Sol identity.

## Install on a phone

1. On the phone: Settings, About phone, tap Build number seven times; then Developer options,
   USB debugging on.
2. Connect it by USB, accept the prompt on the phone, and check it shows in `adb devices`.
3. Install (the same command updates an installed Sol and keeps its data):

   ```sh
   adb install -r android/build/sol-release.apk   # or build/sol.apk for a debug build
   ```

Open Sol and choose **Start new** (a new person on this phone) or **Add to my Sol** (this phone
becomes another device of the person you already are on your laptop).

### Join your laptop

1. On the laptop open Sol (`sol open`), go to **Add my device**, and keep the QR code on screen.
2. On the phone choose **Add to my Sol** and give the phone a name (the model is the default).
3. Scan the QR code with the phone's camera app and tap the `sol://join` link it shows. Sol opens
   and joins at once. Without a camera, copy the link to the phone and paste it into the field.
4. When it says "Joined", the Sol app opens. Files and notes now sync with the laptop. While a
   phone waits for its link, the notification says "Joining…".

If you picked the wrong option, **Start new instead** on that screen deletes the half-made identity
(nothing has been shared yet) and goes back to the welcome screen.

To pair with *another person*, they show **Add a person**; scanning their QR (a `sol://pair` link)
asks "Pair with this person?" and, on Pair, makes you contacts. A link that is not a Sol link, has
expired or was already used shows what to do instead; nothing half-done is left behind.

### Update

Build again and run `adb install -r` with the same kind of APK (release over release, debug over
debug). The node restarts with the app and keeps its identity, contacts and files.

## How it works

- **Node**: `NodeService` runs `libsol.so init` once (the default transports, WireGuard, TLS direct
  and Tor; the UI on 127.0.0.1:7777; files in `SolDrive`), then `libsol.so run` forever,
  restarted with a growing delay if it dies. Data lives in the app's private storage
  (`allowBackup=false`, so the device key never reaches a cloud backup).
- **Tor**: the node runs the APK's own tor; `SOL_TOR_BIN` points it at `libtor.so` in the
  native library directory (the only place an app may execute files from).
- **Login**: the daemon's same-user sign-in needs `/proc/net/tcp`, which Android's SELinux policy
  hides from apps, so the WebView signs in to the Sol app with a one-time code from
  `libsol.so open --print`, and keeps that session across restarts while the node accepts it.
  App origins (Files, Notes) are signed in from that session on first use: the activity makes
  the `/_sol/open` → `/_sol/login?code=` requests itself and stores the origin's `sol_app`
  cookie, because Chromium treats `files.localhost` and `localhost` as different sites and drops
  a SameSite=Strict cookie set through that redirect chain.
- **Network changes**: a new default network (Wi-Fi back, Wi-Fi to cellular) runs
  `libsol.so network-changed`, so peers are redialled and Tor restarted at once. The node's own
  netlink watch is denied to apps.
- **Share**: Sol is a share target for text and files (one or many). They are copied into
  `SolDrive/Inbox-from-share` and Files opens.
- **Downloads** from the WebView are saved to the Downloads folder under the file's own name.

## Test on the emulator

```sh
TC=~/sol-mobile/.toolchain
ANDROID_SDK_ROOT=$TC/sdk ANDROID_AVD_HOME=$TC/avd \
  $TC/sdk/emulator/emulator -avd sol -no-window -no-audio -gpu swangle_indirect &
ABIS=x86_64 android/build.sh && adb install -r android/build/sol.apk
adb shell am start -n app.sol/.MainActivity --es name Ana    # debug builds only: skip the name form
adb shell am start -p app.sol -a android.intent.action.VIEW -d 'sol://pair?...'   # a scanned link
adb emu kill
```

Use `-gpu swangle_indirect`: with `swiftshader_indirect` the emulator's host render thread
segfaults as soon as the Sol app's page draws. A debug build accepts `--es name X` so scripts can
skip the form. Screenshots of the tested flows are in `android/shots/`.
