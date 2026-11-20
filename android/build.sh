#!/usr/bin/env bash
# Builds the Sol Android app (build/sol.apk) with no Gradle: the Go node is cross-compiled and
# shipped as lib/<abi>/libsol.so, the Java is built with javac + d8, the APK is packed with aapt2,
# zipalign and signed with apksigner (debug key).
#
#   android/build.sh               build/sol.apk for arm64-v8a and x86_64
#   ABIS=x86_64 android/build.sh   only the emulator ABI
#
# Needs the toolchain (JDK 17, Android SDK build-tools 35.0.0 and android-35) in TC, default
# ~/sol-mobile/.toolchain; point TC elsewhere to use your own. Nothing is downloaded.
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
ABIS=${ABIS:-"arm64-v8a x86_64"}
VERSION_NAME=${SOL_VERSION:-0.1.0-dev}

export JAVA_HOME=$TC/jdk PATH=$TC/jdk/bin:$PATH
bt=$TC/sdk/build-tools/$BUILD_TOOLS
jar=$TC/sdk/platforms/$PLATFORM/android.jar
[ -x "$bt/aapt2" ] && [ -f "$jar" ] || { echo "toolchain not found in $TC (set TC=...)" >&2; exit 1; }
if [ ! -f "$TC/debug.keystore" ]; then
  keytool -genkeypair -keystore "$TC/debug.keystore" -storepass android -keypass android -alias sol-debug \
    -keyalg RSA -keysize 2048 -validity 10000 -dname "CN=Sol debug build" >/dev/null 2>&1
fi

rm -rf "$OUT" && mkdir -p "$OUT/gen" "$OUT/classes" "$OUT/dex"

for abi in $ABIS; do
  # Go's android/amd64 needs cgo and the NDK, so the x86_64 node (emulator only) is a static
  # linux/amd64 binary, which Android's kernel runs as is. arm64 phones get the real android build.
  case $abi in arm64-v8a) os=android; arch=arm64;; x86_64) os=linux; arch=amd64;; *) echo "unsupported ABI $abi" >&2; exit 1;; esac
  mkdir -p "$OUT/lib/$abi"
  # Programs must be named lib*.so to be unpacked where Android lets
  # an app execute them.
  (cd "$REPO" && CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
     -ldflags "-s -w" -o "$OUT/lib/$abi/libsol.so" ./cmd/sol)
done

"$bt/aapt2" compile --dir app/res -o "$OUT/res.zip"
"$bt/aapt2" link -o "$OUT/unsigned.apk" -I "$jar" --manifest app/AndroidManifest.xml --java "$OUT/gen" \
  --min-sdk-version $MIN_SDK --target-sdk-version $TARGET_SDK --version-name "$VERSION_NAME" --version-code 1 "$OUT/res.zip"
javac -Xlint:-options -source 11 -target 11 -classpath "$jar" -d "$OUT/classes" $(find app/src "$OUT/gen" -name '*.java')
"$bt/d8" --lib "$jar" --min-api $MIN_SDK --output "$OUT/dex" $(find "$OUT/classes" -name '*.class')

(cd "$OUT/dex" && zip -q "$OUT/unsigned.apk" classes.dex)
(cd "$OUT" && zip -q "$OUT/unsigned.apk" $(for abi in $ABIS; do echo lib/$abi/libsol.so; done))
"$bt/zipalign" -f -p 4 "$OUT/unsigned.apk" "$OUT/aligned.apk"
"$bt/apksigner" sign --ks "$TC/debug.keystore" --ks-pass pass:android --key-pass pass:android \
  --out "$OUT/sol.apk" "$OUT/aligned.apk"
"$bt/apksigner" verify "$OUT/sol.apk"
rm -f "$OUT/unsigned.apk" "$OUT/aligned.apk" "$OUT/res.zip" "$OUT/sol.apk.idsig"
echo "built $OUT/sol.apk ($(du -h "$OUT/sol.apk" | cut -f1))"
