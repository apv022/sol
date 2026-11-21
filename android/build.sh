#!/usr/bin/env bash
# Builds the Sol Android app (build/sol.apk) with no Gradle: the Go node is cross-compiled and
# shipped as lib/<abi>/libsol.so next to Tor (lib/<abi>/libtor.so, from the Tor Project's signed
# Android expert bundle), the Java is built with javac + d8, the APK is packed with aapt2,
# zipalign and signed with apksigner (debug key, or yours with RELEASE=1).
#
#   android/build.sh               build/sol.apk for arm64-v8a and x86_64
#   ABIS=x86_64 android/build.sh   only the emulator ABI
#   RELEASE=1 android/build.sh     build/sol-release.apk: debuggable=false, arm64-v8a only, signed with your
#                                  key. Needs SOL_KEYSTORE (a PKCS12/JKS file), SOL_KEY_ALIAS and
#                                  SOL_KEYSTORE_PASS (env only, never argv; also the key password).
#                                  Without them it stops before building. SOL_VERSION sets the name.
#
# Needs the toolchain (JDK 17, Android SDK build-tools 35.0.0 and android-35) in TC, default
# ~/sol-mobile/.toolchain; point TC elsewhere to use your own. The only download is Tor's Android
# bundle (once per ABI, cached in $TC/dl), checked against the Tor Browser signing key before use.
set -euo pipefail
cd "$(dirname "$0")"
HERE=$PWD
REPO=${SOL_SRC:-$(cd .. && pwd)}   # the Go tree to build the node from
TC=${TC:-$HOME/sol-mobile/.toolchain}
OUT=$HERE/build
BUILD_TOOLS=${BUILD_TOOLS:-35.0.0}
PLATFORM=${PLATFORM:-android-35}
MIN_SDK=26
TARGET_SDK=34
RELEASE=${RELEASE:-}
if [ -n "$RELEASE" ]; then
  ABIS=${ABIS:-arm64-v8a}   # phones only: the x86_64 node is a linux binary for the emulator
  case " $ABIS " in *" x86_64 "*) echo "RELEASE=1 builds are arm64-v8a only (ABIS=$ABIS)" >&2; exit 1;; esac
  for v in SOL_KEYSTORE SOL_KEYSTORE_PASS SOL_KEY_ALIAS; do
    [ -n "${!v:-}" ] || { echo "RELEASE=1 needs $v (SOL_KEYSTORE, SOL_KEY_ALIAS and SOL_KEYSTORE_PASS; passwords by env only)" >&2; exit 1; }
  done
  [ -f "$SOL_KEYSTORE" ] || { echo "RELEASE=1: keystore $SOL_KEYSTORE not found" >&2; exit 1; }
else
  ABIS=${ABIS:-"arm64-v8a x86_64"}
fi
VERSION_NAME=${SOL_VERSION:-0.1.0-dev}
TOR_BROWSER=16.0.2   # dist.torproject.org/torbrowser/<this>/ has tor-expert-bundle-android-<arch>-<this>.tar.gz
# The Tor Browser Developers signing key (support.torproject.org/tbb/how-to-verify-signature/).
TOR_KEY=EF6E286DDA85EA2A4BA7DE684E2C6E8793298290

export JAVA_HOME=$TC/jdk PATH=$TC/jdk/bin:$PATH
bt=$TC/sdk/build-tools/$BUILD_TOOLS
jar=$TC/sdk/platforms/$PLATFORM/android.jar
[ -x "$bt/aapt2" ] && [ -f "$jar" ] || { echo "toolchain not found in $TC (set TC=...)" >&2; exit 1; }
if [ -z "$RELEASE" ] && [ ! -f "$TC/debug.keystore" ]; then
  keytool -genkeypair -keystore "$TC/debug.keystore" -storepass android -keypass android -alias sol-debug \
    -keyalg RSA -keysize 2048 -validity 10000 -dname "CN=Sol debug build" >/dev/null 2>&1
fi

APK=$OUT/sol.apk
[ -n "$RELEASE" ] && APK=$OUT/sol-release.apk
# Start clean, but keep the other kind of APK (a debug build leaves sol-release.apk alone).
rm -rf "$OUT/gen" "$OUT/classes" "$OUT/dex" "$OUT/lib" "$APK" && mkdir -p "$OUT/gen" "$OUT/classes" "$OUT/dex"

# tor_for ABI DEST: Tor's Android build for ABI, copied to DEST. The bundle is downloaded once into
# $TC/dl and checked on every build: its own signature and the signed checksum list must both be
# by TOR_KEY (gpg in a throwaway GNUPGHOME, so no keyring of yours is read or changed), and its
# hash must be the one in that list. The key itself is fetched by WKD once and cached; it is
# trusted only because its fingerprint is pinned above.
tor_for() {
  local arch; case $1 in arm64-v8a) arch=aarch64;; x86_64) arch=x86_64;; esac
  local base=https://dist.torproject.org/torbrowser/$TOR_BROWSER f=tor-expert-bundle-android-$arch-$TOR_BROWSER.tar.gz
  local dl=$TC/dl sums=sha256sums-signed-build.txt
  mkdir -p "$dl"
  for x in "$f" "$f.asc" "$sums" "$sums.asc"; do
    [ -s "$dl/$x" ] && continue
    curl -fsSL -o "$dl/$x.part" "$base/$x" || { echo "cannot download $base/$x" >&2; exit 1; }
    mv "$dl/$x.part" "$dl/$x"
  done
  local gh; gh=$(mktemp -d); chmod 700 "$gh"
  if [ ! -s "$dl/torbrowser-key.asc" ]; then
    GNUPGHOME=$gh gpg --batch --quiet --auto-key-locate nodefault,wkd --locate-keys torbrowser@torproject.org >/dev/null 2>&1 || true
    GNUPGHOME=$gh gpg --batch --export --armor "$TOR_KEY" > "$dl/torbrowser-key.asc" 2>/dev/null || true
  fi
  GNUPGHOME=$gh gpg --batch --quiet --import "$dl/torbrowser-key.asc" >/dev/null 2>&1 || true
  local sig
  for sig in "$f" "$sums"; do
    GNUPGHOME=$gh gpg --batch --status-fd 1 --verify "$dl/$sig.asc" "$dl/$sig" 2>/dev/null \
      | grep -q "^\[GNUPG:\] VALIDSIG .* $TOR_KEY\$" \
      || { rm -rf "$gh"; rm -f "$dl/torbrowser-key.asc"; echo "$sig: no valid signature by $TOR_KEY" >&2; exit 1; }
  done
  rm -rf "$gh"
  (cd "$dl" && grep "  $f\$" "$sums" | sha256sum -c --quiet -) || { echo "$f: checksum mismatch" >&2; exit 1; }
  # libTor.so is a self-contained program (no shared libraries beyond Android's libc).
  tar -xzOf "$dl/$f" tor/libTor.so > "$2"
  chmod 755 "$2"
}

for abi in $ABIS; do
  # Go's android/amd64 needs cgo and the NDK, so the x86_64 node (emulator only) is a static
  # linux/amd64 binary, which Android's kernel runs as is. arm64 phones get the real android build.
  case $abi in arm64-v8a) os=android; arch=arm64;; x86_64) os=linux; arch=amd64;; *) echo "unsupported ABI $abi" >&2; exit 1;; esac
  mkdir -p "$OUT/lib/$abi"
  # Programs must be named lib*.so to be unpacked where Android lets
  # an app execute them.
  (cd "$REPO" && CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
     -ldflags "-s -w" -o "$OUT/lib/$abi/libsol.so" ./cmd/sol)
  tor_for "$abi" "$OUT/lib/$abi/libtor.so"
done

"$bt/aapt2" compile --dir app/res -o "$OUT/res.zip"
# The checked-in manifest is the debug one; a release build flips debuggable off in a copy.
MANIFEST=app/AndroidManifest.xml
if [ -n "$RELEASE" ]; then
  MANIFEST=$OUT/AndroidManifest.xml
  sed 's/android:debuggable="true"/android:debuggable="false"/' app/AndroidManifest.xml > "$MANIFEST"
  grep -q 'debuggable="false"' "$MANIFEST" || { echo "could not make the manifest non-debuggable" >&2; exit 1; }
fi
"$bt/aapt2" link -o "$OUT/unsigned.apk" -I "$jar" --manifest "$MANIFEST" --java "$OUT/gen" \
  --min-sdk-version $MIN_SDK --target-sdk-version $TARGET_SDK --version-name "$VERSION_NAME" --version-code 1 "$OUT/res.zip"
javac -Xlint:-options -source 11 -target 11 -classpath "$jar" -d "$OUT/classes" $(find app/src "$OUT/gen" -name '*.java')
"$bt/d8" --lib "$jar" --min-api $MIN_SDK --output "$OUT/dex" $(find "$OUT/classes" -name '*.class')

(cd "$OUT/dex" && zip -q "$OUT/unsigned.apk" classes.dex)
(cd "$OUT" && zip -q "$OUT/unsigned.apk" $(for abi in $ABIS; do echo lib/$abi/libsol.so lib/$abi/libtor.so; done))
"$bt/zipalign" -f -p 4 "$OUT/unsigned.apk" "$OUT/aligned.apk"
if [ -n "$RELEASE" ]; then
  # Passwords reach apksigner through the environment, so they never show in `ps`.
  "$bt/apksigner" sign --ks "$SOL_KEYSTORE" --ks-pass env:SOL_KEYSTORE_PASS --key-pass env:SOL_KEYSTORE_PASS \
    --ks-key-alias "$SOL_KEY_ALIAS" --out "$APK" "$OUT/aligned.apk"
else
  "$bt/apksigner" sign --ks "$TC/debug.keystore" --ks-pass pass:android --key-pass pass:android \
    --out "$APK" "$OUT/aligned.apk"
fi
"$bt/apksigner" verify "$APK"
rm -f "$OUT/AndroidManifest.xml"
rm -f "$OUT/unsigned.apk" "$OUT/aligned.apk" "$OUT/res.zip" "$APK.idsig"
echo "built $APK ($(du -h "$APK" | cut -f1))"
