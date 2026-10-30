#!/usr/bin/env bash
# Builds bootadb/out/bootadb.apk with the Android SDK tools only (no Gradle, no libraries).
# Needs ANDROID_HOME (GitHub's Ubuntu runners have it), a JDK and zip.
#
# Signing: set BOOTADB_KEYSTORE to a PKCS#12 file (and BOOTADB_KEYSTORE_PASS to its password) to
# sign every build with the same certificate, which is what lets Android update the helper on a TV
# over the top of the previous build. Without it this falls back to a throw-away key made here and
# now, which still produces a working APK but forces tv-patcher to remove the app before it can
# install the new one — losing its permission grant, its Doze exemption and both its jobs.
#
# The key is not secret in any interesting way: tv-patcher identifies its own helper by what the
# APK installs (apk.InstallsID, every entry's CRC and size), never by who signed it, and abusing
# the key would need ADB access to the TV, where the shell already holds WRITE_SECURE_SETTINGS.
# It is kept out of git anyway, because a private key in a public repository is a bad habit to
# teach regardless of whether this particular one matters, and losing it costs only one more
# replace-instead-of-update per TV. CI holds it as the BOOTADB_KEYSTORE_B64 repository secret.
set -euo pipefail

cd "$(dirname "$0")"
: "${ANDROID_HOME:?set ANDROID_HOME to the Android SDK}"

BT="$ANDROID_HOME/build-tools/$(ls "$ANDROID_HOME/build-tools" | grep -E '^[0-9.]+$' | sort -V | tail -1)"
PLATFORM="$ANDROID_HOME/platforms/$(ls "$ANDROID_HOME/platforms" | grep -E '^android-[0-9]+(\.[0-9]+)?$' | sort -V | tail -1)" # newest stable platform (no beta)
API=${PLATFORM##*android-}
API=${API%%[!0-9]*}
# The version is the integer in ./version, bumped by hand when the helper changes. It used to be a
# hash of the sources, which kept a rebuild of unchanged code at the same version (so the weekly
# image refresh touches no TV) but could move in either direction -- and Android refuses to install
# a lower versionCode over a higher one, which forced every update to be an uninstall. This keeps
# the first property and adds the monotonicity. Forgetting to bump it is caught in CI, and even
# unnoticed it only costs a fallback to uninstall-then-install, never a TV left out of date.
VERSION_CODE=${VERSION_CODE:-$(cat version)}
case $VERSION_CODE in
  ''|*[!0-9]*) echo "bootadb/version must be a plain integer, not '$VERSION_CODE'" >&2; exit 1 ;;
esac
if [ "$VERSION_CODE" -lt 1 ] || [ "$VERSION_CODE" -gt 2100000000 ]; then
  echo "bootadb/version must be between 1 and 2100000000, not $VERSION_CODE" >&2; exit 1
fi
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
KS=${BOOTADB_KEYSTORE:-}
KS_PASS=${BOOTADB_KEYSTORE_PASS:-bootadb}
if [ -n "$KS" ] && [ -r "$KS" ]; then
  echo "signing with $KS, so TVs can take this as an update"
else
  echo "no BOOTADB_KEYSTORE: signing with a throw-away key, so every TV must replace the helper" >&2
  KS=out/ks.p12
  KS_PASS=bootadb
  keytool -genkeypair -keystore "$KS" -storetype PKCS12 -alias k -keyalg RSA -keysize 2048 \
    -validity 10000 -dname "CN=tv-patcher bootadb" -storepass "$KS_PASS" -keypass "$KS_PASS" 2>/dev/null
fi
"$BT/apksigner" sign --ks "$KS" --ks-key-alias k --ks-pass "pass:$KS_PASS" \
  --out out/bootadb.apk out/aligned.apk
"$BT/apksigner" verify out/bootadb.apk
rm -f out/base.apk out/aligned.apk out/ks.p12 out/*.idsig
ls -l out/bootadb.apk
