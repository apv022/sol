#!/usr/bin/env bash
# Runs the Android shell's pure-logic tests on a plain JDK (no emulator, no android.jar): the
# classes listed in SRC have no Android types, and each test is a class with a main that exits
# non-zero on failure. Uses the toolchain's JDK (TC, default ~/sol-mobile/.toolchain) or PATH's.
set -euo pipefail
cd "$(dirname "$0")"
TC=${TC:-$HOME/sol-mobile/.toolchain}
[ -x "$TC/jdk/bin/javac" ] && export PATH=$TC/jdk/bin:$PATH
SRC="../app/src/app/sol/WifiDirectPolicy.java"
OUT=$(mktemp -d)
trap 'rm -rf "$OUT"' EXIT
javac -Xlint:-options -source 11 -target 11 -d "$OUT" $SRC $(find app -name '*Test.java')
for t in $(cd app && find . -name '*Test.java' | sed 's|^\./||; s|\.java$||; s|/|.|g'); do
  java -cp "$OUT" "app.$t"
done
