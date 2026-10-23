#!/usr/bin/env bash
# Builds bootadb/out/bootadb.apk with the Android SDK tools only (no Gradle, no libraries).
# Needs ANDROID_HOME (GitHub's Ubuntu runners have it), a JDK and zip.
# The APK is signed with a throw-away key made for this build: tv-patcher uninstalls and
# reinstalls the helper when the version changes, so no long-lived signing key is needed.
set -euo pipefail

cd "$(dirname "$0")"
: "${ANDROID_HOME:?set ANDROID_HOME to the Android SDK}"

BT="$ANDROID_HOME/build-tools/$(ls "$ANDROID_HOME/build-tools" | grep -E '^[0-9.]+$' | sort -V | tail -1)"
PLATFORM="$ANDROID_HOME/platforms/$(ls "$ANDROID_HOME/platforms" | grep -E '^android-[0-9]+(\.[0-9]+)?$' | sort -V | tail -1)" # newest stable platform (no beta)
API=${PLATFORM##*android-}
API=${API%%[!0-9]*}
# The version comes from the app's own files, so rebuilding the same code (the weekly image
# refresh) gives the same version and tv-patcher leaves the TV alone; a real change gives a new one.
if command -v sha256sum >/dev/null; then HASH=sha256sum; else HASH="shasum -a 256"; fi
SUM=$( (cat AndroidManifest.xml build.sh; find src -name '*.java' | LC_ALL=C sort | xargs cat) | $HASH | cut -c1-7)
VERSION_CODE=${VERSION_CODE:-$(( 16#$SUM % 2000000000 + 1 ))}
echo "build-tools $(basename "$BT"), platform $(basename "$PLATFORM"), versionCode $VERSION_CODE"

rm -rf out && mkdir -p out/classes out/dex
JAR="$PLATFORM/android.jar"

"$BT/aapt2" link -o out/base.apk -I "$JAR" --manifest AndroidManifest.xml \
  --min-sdk-version 26 --target-sdk-version "$API" \
  --version-code "$VERSION_CODE" --version-name "$VERSION_CODE"

javac --release 11 -Xlint:-options -classpath "$JAR" -d out/classes src/com/sloccy/bootadb/*.java
"$BT/d8" --release --min-api 26 --lib "$JAR" --output out/dex $(find out/classes -name '*.class')
(cd out/dex && zip -q ../base.apk classes.dex)

"$BT/zipalign" -f 4 out/base.apk out/aligned.apk
keytool -genkeypair -keystore out/ks.p12 -storetype PKCS12 -alias k -keyalg RSA -keysize 2048 \
  -validity 10000 -dname "CN=tv-patcher bootadb" -storepass bootadb -keypass bootadb 2>/dev/null
"$BT/apksigner" sign --ks out/ks.p12 --ks-pass pass:bootadb --key-pass pass:bootadb \
  --out out/bootadb.apk out/aligned.apk
"$BT/apksigner" verify out/bootadb.apk
rm -f out/base.apk out/aligned.apk out/ks.p12 out/*.idsig
ls -l out/bootadb.apk
