#!/usr/bin/env bash
# Builds the M5 test client (build/sol-testclient.apk): one activity that connects to Sol with
# app.sol.action.CONNECT and calls /v1/whoami. Test only: never part of a release. Same
# toolchain as ../build.sh (TC, default ~/sol-mobile/.toolchain), signed with its debug key.
set -euo pipefail
cd "$(dirname "$0")"
TC=${TC:-$HOME/sol-mobile/.toolchain}
OUT=$PWD/build
bt=$TC/sdk/build-tools/${BUILD_TOOLS:-35.0.0}
jar=$TC/sdk/platforms/${PLATFORM:-android-35}/android.jar
export JAVA_HOME=$TC/jdk PATH=$TC/jdk/bin:$PATH
[ -x "$bt/aapt2" ] && [ -f "$jar" ] || { echo "toolchain not found in $TC (set TC=...)" >&2; exit 1; }
[ -f "$TC/debug.keystore" ] || { echo "run ../build.sh once first (it makes the debug key)" >&2; exit 1; }
rm -rf "$OUT" && mkdir -p "$OUT/classes" "$OUT/dex"
"$bt/aapt2" link -o "$OUT/unsigned.apk" -I "$jar" --manifest AndroidManifest.xml --min-sdk-version 26 --target-sdk-version 34
javac -Xlint:-options -source 11 -target 11 -classpath "$jar" -d "$OUT/classes" $(find src -name '*.java')
"$bt/d8" --lib "$jar" --min-api 26 --output "$OUT/dex" $(find "$OUT/classes" -name '*.class')
(cd "$OUT/dex" && zip -q "$OUT/unsigned.apk" classes.dex)
"$bt/zipalign" -f -p 4 "$OUT/unsigned.apk" "$OUT/aligned.apk"
"$bt/apksigner" sign --ks "$TC/debug.keystore" --ks-pass pass:android --key-pass pass:android --out "$OUT/sol-testclient.apk" "$OUT/aligned.apk"
rm -f "$OUT/unsigned.apk" "$OUT/aligned.apk" "$OUT/sol-testclient.apk.idsig"
echo "built $OUT/sol-testclient.apk"
