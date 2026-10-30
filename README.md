# TV Patcher

Keeps Morphe-patched apps (Prime Video, Netflix, Disney+, and anything else the
[morphe-androidtv-patches](https://github.com/ajstrick81/morphe-androidtv-patches) support) installed
on your Google TVs. A small web page lets you add devices and tick apps; a scheduler does the rest:

1. find the newest patches release and the newest stable app version it supports;
2. get that exact Android TV bundle (your drop folder, the cache, APKMirror, APKMirror through
   FlareSolverr) and verify it;
3. patch it once with the Morphe CLI for the TV's CPU;
4. install it over wireless ADB on every device that needs it, and tell you on Telegram. A new patches
   release that does not change a given app is noticed and that app is not re-installed at all. A scheduled
   run leaves a TV alone while something is playing on it and tries again at the next run; **Run now**
   always installs. After a successful update the older versions' downloads and patched files are deleted.
   If the patches ever
   roll the target back to a version older than the one installed, the older build is still installed (the app is uninstalled first, so you log in again; `allow_uninstall: false` opts an app out). Only a patched app identical to the one already on the TV is left alone.
   A bundle already on disk is never downloaded again: the cache is not size-capped, and a cached file
   is used without visiting APKMirror whenever the release offers no better build for the TV.

Written in Go; the image adds the Java runtime, `adb` and `apksigner`.

## Run it

```yaml
services:
  tv-patcher:
    image: ghcr.io/sloccy/morphe-tv-patcher:latest
    network_mode: host            # adb reaches the TV by LAN IP; the page is on :8093
    restart: unless-stopped
    env_file: /srv/secrets/telegram.env   # TELEGRAM_BOT_TOKEN, TELEGRAM_CHAT_ID (optional)
    environment:
      FLARESOLVERR_URL: "http://127.0.0.1:8191"   # optional Cloudflare fallback
    volumes:
      - tv_patcher:/data
volumes:
  tv_patcher:
```

Open `http://<host>:8093`, add a device (its IP), press **Pair…** and follow the prompt, tick apps,
**Save**, then **Run now**. Saving never installs anything; the schedule (default every 6 hours) and
the button do.

**No login.** Like your other LAN tools, the page is meant for the LAN/Tailscale only: anyone who can
reach it can add devices and trigger installs. Do not expose it publicly. Requests from other web
pages are refused (JSON-only, matching `Origin`, and a `Host` allow-list: set
`ALLOWED_HOST_SUFFIXES=.example.com` if you reach it by a name).

## On the TV

Settings → System → About → click **Build** 7 times, then Developer options → **Wireless debugging**
on. Give the TV a fixed IP. Google TV picks a random port that changes; the patcher finds it (last
good port, mDNS, then a port scan). Pair once from the page ("Pair device with pairing code"); the
keys live in the data volume.

Apps first installed by Morphe Manager are signed with Manager's key, so Android refuses to update
them, so tv-patcher uninstalls the app and reinstalls it (you log in again). The same goes for a
downgrade, which Android refuses without an uninstall. Set `allow_uninstall` to `false` for an app in
`settings.json` to opt out (you are told to uninstall by hand). Never uninstall stock Netflix; the patched copy installs beside it as a clone.

## Several TVs and app sets

Add each TV on the page and pair it once (**Pair…**; the page then reads its CPU, so the card shows
"paired" and, for example, "32-bit · armeabi-v7a, armeabi"). Every TV is checked and updated on its own.

To give the same apps to several TVs, create an **app set** (a name plus apps, each with its patch
repository) and tick the TVs that use it, either under App sets ("Used by") or on a TV's card. Edit the
set once and every TV using it follows on its next run. A TV can use several sets and also have its own
extra apps (those win if they clash); apps that come from a set show locked with "from <set>". "Make a
set from these apps" turns a TV you have already configured into a set.

**Saves are refused when a TV cannot run an app.** Before a set or a TV's apps are saved, each affected
TV is checked against the CPU builds the app offers at its newest supported version, which are read from
APKMirror's release page (no download) and remembered. If the page says, for example, that Netflix only
has 32-bit builds and a TV runs arm64 only, the save is rejected with that reason and nothing changes.
When something cannot be checked (a TV that has never been reached, or APKMirror not answering) the save
goes through, the page says what was not checked, and a mismatch then appears as the usual "no build for
this CPU" message on that TV. Dropping an app from a set never uninstalls it.

## More patch repositories

No repository is built in as a requirement. On the very first run the **Patch sources** list is seeded
from `PATCHES_REPO` (default `ajstrick81/morphe-androidtv-patches`, the Android TV patches; a comma
separated list is fine). After that the list lives in `settings.json`, every source can be removed
or added from the page, and a removed one stays removed across restarts. The page adds any GitHub
repository that publishes Morphe bundles (`.mpp` release assets), for example `MorpheApp/morphe-patches`
(YouTube, YouTube Music, Reddit). Its apps then appear on every device card.

**If a repository disappears** (renamed, deleted, down), nothing breaks: each source's last good bundle
is kept in `/data/patches/<source>/` and used while the repository is unreachable. The page shows a
warning on that source, you get one Telegram message after the failure repeats, and you can remove it.
Apps already on the TVs keep working; only updates from that repository stop. Because `/data` is the
small state volume that is backed up, `settings.json` (your repositories, devices and apps), the signing
keystore and those last good bundles all come back from a restore. If two repositories
support the same app, a dropdown on that app picks which repository's patches a device uses (one per
app per device; bundles are never combined). Apps from a repository that is marked "allow phone apps"
skip the Android TV check (they may install but not show in the TV launcher); everything else keeps it.

**Trust model.** Patches are code that runs in the Morphe JVM on this server, which holds the signing
key and the adb pairing keys, and the patched apps are then installed on your TVs. The page has no
login, so anyone who can reach it can add a repository; adding one requires ticking a trust
confirmation, and every addition and removal is logged. To limit who can be added, set
`PATCH_SOURCES_ALLOWED_OWNERS=ajstrick81,MorpheApp` (comma separated GitHub owners); empty allows any.
Bundles are only taken from GitHub release assets, at most 100 MB.

## CPU architectures

Each device reports its CPU list (for example `arm64-v8a,armeabi-v7a,armeabi` for a 64-bit TV box,
`armeabi-v7a,armeabi` for a 32-bit one) and the page shows it on the card. Builds are chosen to match:
a native 64-bit build is preferred for a 64-bit-capable device, a 32-bit build is used when that is all
there is, and a device only gets a build it can run. Devices with the same list share one download and
one patched file; a different list gets its own. If an app has no build for a device's CPU (for example
Netflix on an arm64-only box, since the patches only exist for 32-bit builds) the page says so once.

## Where APKs come from

In order, the first file that passes every check wins:

1. your drop folder `/data/work/input/<package>/` (any `.apkm`, `.xapk`, `.apk`);
2. the cache;
3. APKMirror, directly. Every APKMirror request (from runs and from background CPU checks) goes through one shared gate: one request at a time, a few seconds apart, and a shared pause (5 minutes, doubling up to an hour) as soon as the site says "slow down";
4. APKMirror through a FlareSolverr sidecar (`FLARESOLVERR_URL`);

Which CPU builds an app offers is learned in the background, one app at a time, only for apps you selected. Saving waits a few seconds for it; anything still unknown is saved anyway and shows up as a warning on the TV if it turns out the TV cannot run it. The page says "Checking which CPUs…" while this runs.

The APKMirror page for an app is found automatically (search by package name, an Android TV page
first) and cached; `settings.json` can override a wrong guess. Nothing is downgraded: if the newest
supported version cannot be found you are told, with the exact file to drop in.

**A patches release that changes nothing is not re-installed.** A new bundle usually only changes the
patches of one or two apps, but its release tag changes for all of them, and nothing in the bundle says
which app a change belongs to. So every selected app is still patched once per release, and the result
is then compared with what the TV already has. Not by hashing the file: the patcher rebuilds the archive
and signs it again every run, so two identical builds are never byte-identical. What is hashed is what
the APK *installs* — every entry's name, checksum and size, plus the signing certificate. If that matches,
the TV is left alone: no ADB session, no notice on the screen, no restart of the app, no Telegram message.
The record is updated with the new release, so the next scheduled run does not even download. A different
signing key (a regenerated keystore) counts as a different build and is installed. Because the comparison
goes by the record, an app taken off a TV by hand is noticed on the next **Run now** or within
`VERIFY_EVERY`, not immediately. `tv-patcher digest FILE [FILE...]` prints that identity for any APK, which
is how you can check for yourself that patching the same input twice really does produce the same app.

**The same inputs are not patched twice.** What a build depends on — the app file, the patch bundle, the
CPU list, `BYTECODE_MODE`, the keystore and the per-app `enable`/`disable`/`options_file` — is remembered
together with the file it produced. A later run with the same inputs (a dry run, a TV that was unreachable
last cycle, a newly added TV) reuses that file instead of spending minutes patching again; `--force`
always rebuilds. Changing `BYTECODE_MODE` or an override now rebuilds, where before it changed nothing.

**Nothing is stripped:** patched apps keep every native-library architecture (no `--striplibs`), because
dropping one can leave an app that will not install or starts and closes. The files are larger.

**Every file is verified:** right package and version, an Android TV launcher (rejects phone builds),
`armeabi-v7a`, and the publisher's signing certificate: learned from the first APKMirror download,
or pinned with `signer_sha256`; `apksigner` checks it cryptographically. **Every APK inside a bundle**
is checked, not just `base.apk`: Morphe merges the splits into the patched app, so each one must carry
the base's certificate and pass `apksigner`, or the bundle is refused. A bundle that names the same
entry twice is refused outright (Go would read the first copy and the Java patcher the last, so the
file verified need not be the file patched). Without `apksigner` on `PATH` nothing can be checked
cryptographically and files are refused; `ALLOW_UNVERIFIED_SIGNATURES=true` accepts them anyway
(certificates are still compared), for runs outside the image.

**Connections to the TV.** Android shows a "debugging connected" notice on the TV (and it can pull you
out of a movie) for every ADB session, so tv-patcher connects as little as it can. A scheduled run
plans from its own records and connects to a TV only when something has to be installed, when the TV
has not been checked for `VERIFY_EVERY` (default 7 days, which also notices an app you removed by
hand, and is shortened to 5 days for a TV that still expires ADB keys), or the first time. The "something is playing" check happens just before installing, not before
downloading, so a release that changes nothing for an app never reaches the TV at all. **Run now** always connects. Each TV card has two test buttons: **Test**
only looks at the network (no ADB session, so no notice) and says whether Wireless debugging is
answering; **Full test** really connects, checks the pairing, reads the CPU and updates the boot helper.
The app list on each card can be collapsed (the line above it still says how many apps the TV gets).

**Boot helper.** Android turns Wireless debugging off when a TV restarts — and at other times of its
own (see below). `bootadb/` is a tiny app (no libraries, no network access) that switches it back on
at every boot, within seconds of Android switching it off, and every 15 minutes as a backstop (it
only changes a setting when it finds it different). The fast path is a JobScheduler content trigger on
the `adb_wifi_enabled` setting itself; the 15-minute job is the guaranteed one, because a content
trigger on a system setting is not a documented contract. It also keeps
`adb_allowed_connection_time` at `0`, which is what stops a TV unpairing itself, and
`development_settings_enabled` at `1`, because turning Developer options off takes Wireless debugging
with it. Those first two matter too much to leave to a background job, so tv-patcher also writes them
itself over ADB whenever it connects and believes only what reads back: the helper's job is started,
not waited for, and a helper nobody ever opens can have its job deferred for hours by Android's
standby bucket. It keeps these tracking settings off: `wifi_scan_always_enabled`, `network_recommendations_enabled`,
`upload_apk_enable`, `package_verifier_user_consent` (-1), `send_action_app_error` and
`location_mode`. It shows no messages; see `adb logcat -s BootAdb`. Whenever tv-patcher connects
to a TV it installs the helper (and replaces it when the image carries a newer one, or when the copy
on the TV is not byte-for-byte the one it ships — the permission is only ever granted to its own
APK), grants it the one permission it needs over ADB, and opens it once so Android lets it receive
the boot broadcast. Opening it does nothing by itself: the activity is reachable by any app on the
TV, so it only schedules the jobs, which tv-patcher then runs once. Because nothing on the TV ever
opens the helper, Android would file it under "rare" and defer its jobs for hours, so tv-patcher also
puts it on the Doze whitelist and in the active standby bucket whenever it connects. A card
shows "boot helper ✓" and "pairing kept ✓". It is built in CI (`bootadb/build.sh`, Android SDK tools only), signed with a
throw-away key, and shipped inside the image at `/opt/bootadb/bootadb.apk` (`BOOT_HELPER_APK`). If a
TV refuses it you get a message on the card and, on the second run in a row, a Telegram alert; apps
are updated regardless.

**Why a TV stops answering.** Three different things, with three different fixes, and all three are
things Android does on purpose:

1. **It unpairs itself after a week.** Android deletes an ADB key that has not connected for
   `adb_allowed_connection_time`, which defaults to 604800000 ms — 7 days. A TV that simply had no
   app to update is never connected to, so the key quietly expires and the next run is refused.
   tv-patcher sets that setting to `0` (keys never expire) over ADB every time it connects, and the
   boot helper keeps it there for the times tv-patcher is not connected. Until a TV confirms it
   reads `0`, tv-patcher connects to it at least every 5 days whatever `VERIFY_EVERY` says, so the
   key is refreshed inside the week; the card shows "expires pairings" for such a TV. If yours has
   already unpaired, press **Pair…** once more — it will not happen again.
2. **Android switches Wireless debugging off by itself.** Not only on a restart: it writes
   `adb_wifi_enabled = 0` when Wi-Fi is turned off, when the network drops, and **when the access
   point changes** ("Detected wifi network change"), so a TV roaming between mesh access points on
   one SSID is enough to do it. The helper puts it straight back, now within seconds rather than
   within the hour. Giving the TV one fixed access point (no band steering) avoids the churn.
3. **The port changes every time.** adbd picks a new random port whenever Wireless debugging comes
   back on, so the remembered one is dead. tv-patcher asks the TV's own mDNS advertisement for the
   new port, which takes milliseconds; a port it could not reach is forgotten rather than dialled
   first forever, and pairing takes the port straight from the adb server.

A TV that cannot be reached for two runs in a row sends a Telegram message saying to turn Wireless debugging back on (a reboot switches it off; the pairing is kept). A TV that answers but no longer accepts this server says so at once instead, because that needs somebody at the TV.

**Finding the port.** In order: the last port that worked, the TV's own mDNS advertisement
(`_adb-tls-connect._tcp`, queried by tv-patcher itself — the `adb` in the image is built without mDNS
support, so `adb mdns services` finds nothing there), and then a scan of `ADB_SCAN_RANGE`
(`30000-61000`, which covers the whole range the kernel hands adbd). The **Pair…** form fills the
pairing port in from the same source, so only the six-digit code has to be typed.

**A port that never changes (opt-in).** `TV_FIXED_ADB_PORT=5555` moves the TV to legacy ADB over TCP
on a fixed port (`persist.adb.tcp.port`, read by adbd at every boot), so no port ever has to be
found. It is off by default and worth understanding before switching on: legacy ADB over TCP is
**not encrypted**, where the wireless-debugging transport is, and it leaves a well-known port
listening on your LAN — still key-authenticated, but 5555 is a standard thing to scan for. Whether
the property survives a restart depends on the TV's firmware, so tv-patcher reads it back and says
on the card what actually stuck; normal discovery stays in place either way. A run only sets the
property (restarting adbd would drop the connection the installs need); **Full test** also restarts
adbd so it takes effect at once.

Temporary download failures alert on Telegram only after 3 cycles in a row (then at most daily); a
wrong signature or a phone build alerts at once; a message follows when it recovers.

## Configuration (environment)

| Variable | Default | |
|---|---|---|
| `DATA_DIR` | `/data` | settings, state, keystore, cache |
| `INTERVAL` | `6h` | time between scheduled runs |
| `WEB_PORT` / `WEB_BIND` | `8093` / `0.0.0.0` | |
| `ALLOWED_HOST_SUFFIXES` | | extra `Host` names the page answers to (whole labels: `example.com` and `.example.com` both match `tv.example.com`, never `evil-example.com`) |
| `FLARESOLVERR_URL` | | e.g. `http://127.0.0.1:8191` |
| `APKMIRROR_MIN_INTERVAL` | `4s` | minimum gap between APKMirror requests |
| `APKMIRROR_COOLDOWN` | `5m` | first pause after APKMirror pushes back (doubles, up to 1h) |
| `VERIFY_EVERY` | `168h` | how often a scheduled run connects to a TV only to check what is installed; capped at 120h for a TV that has not confirmed `adb_allowed_connection_time=0` |
| `ADB_SCAN_RANGE` | `30000-61000` | ports the last-resort scan tries when mDNS is silent |
| `TV_FIXED_ADB_PORT` | `0` | pin ADB to this port (0 = off); unencrypted and LAN-visible, read the section above |
| `PATCHES_REPO` / `PATCHES_PRERELEASE` | `ajstrick81/morphe-androidtv-patches` / `false` | seeds the patch sources on the first run only |
| `PATCH_SOURCES_ALLOWED_OWNERS` | | limit repositories added from the page to these GitHub owners |
| `BYTECODE_MODE` | `STRIP_SAFE` | Morphe `--bytecode-mode` |
| `FETCH_FAILURES_BEFORE_ALERT` | `3` | |
| `ALLOW_UNVERIFIED_SIGNATURES` | `false` | accept app files when `apksigner` is not installed |
| `TELEGRAM_BOT_TOKEN` / `TELEGRAM_CHAT_ID` | | notifications |
| `GITHUB_TOKEN` | | optional, avoids API rate limits |

`/data/state/settings.json` holds your patch sources, devices and apps (edited by the page) plus optional per-app
`overrides`: `apkmirror` (publisher/app path), `sources`, `signer_sha256`, `enable`/`disable` (Morphe
patch names), `options_file`, `allow_uninstall`.

**Back up the signing keystore** (`/data/keystore/morphe.keystore`, in the data volume). Lose it and
every app has to be uninstalled before it can update.

## Commands

```
tv-patcher run                  web page + scheduler (default)
tv-patcher once [--device ID] [--app PKG,PKG] [--force] [--dry-run] [--no-device [--source ID] [--abi LIST]]
tv-patcher pair --device ID --pair-port N --code 123456
tv-patcher fetch --package PKG [--version V] [--source ID] [--abi LIST]   download and verify one app
tv-patcher resolve [--package PKG] [--source ID] find apps on APKMirror, no download
tv-patcher digest FILE [FILE...]  what an APK installs; with two files, whether they are the same app
tv-patcher healthcheck | version
```

Force a run in a container: `docker compose exec tv-patcher tv-patcher once --force`
(or press **Run now** with "reinstall everything" ticked).

## Development

```
go test -race ./...      # unit tests (real saved APKMirror pages and real app manifests)
golangci-lint run        # same config as Lantern
```

CI (`build.yml`) lints, tests, builds amd64+arm64 natively, publishes to GHCR and smoke-tests the
image. `security.yml` calls `sloccy/shared-ci` (nightly govulncheck fixes, weekly rebuild that picks
up new Morphe releases); it needs the repo secret `WORKFLOW_TOKEN`. `live-checks.yml` is
a manual workflow that proves FlareSolverr and app discovery work from a runner.
