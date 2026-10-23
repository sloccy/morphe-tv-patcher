# TV Patcher

Keeps Morphe-patched apps (Prime Video, Netflix, Disney+, and anything else the
[morphe-androidtv-patches](https://github.com/ajstrick81/morphe-androidtv-patches) support) installed
on your Google TVs. A small web page lets you add devices and tick apps; a scheduler does the rest:

1. find the newest patches release and the newest app version it supports;
2. get that exact Android TV bundle (your drop folder, the cache, APKMirror, APKMirror through
   FlareSolverr) and verify it;
3. patch it once with the Morphe CLI for the TV's CPU;
4. install it over wireless ADB on every device that needs it, and tell you on Telegram.

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
them: uninstall them once (the page tells you), or set `allow_uninstall` for that app in
`settings.json`. Never uninstall stock Netflix; the patched copy installs beside it as a clone.

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

**Every file is verified:** right package and version, an Android TV launcher (rejects phone builds),
`armeabi-v7a`, and the publisher's signing certificate: learned from the first APKMirror download,
or pinned with `signer_sha256`; `apksigner` checks it cryptographically.

Each TV card has a **Test** button that connects the way a run does and shows the port and CPU, or tells you to turn Wireless debugging back on. The app list on each card can be collapsed (the line above it still says how many apps the TV gets).

A TV that cannot be reached for two runs in a row sends a Telegram message saying to turn Wireless debugging back on (a reboot switches it off; the pairing is kept).

Temporary download failures alert on Telegram only after 3 cycles in a row (then at most daily); a
wrong signature or a phone build alerts at once; a message follows when it recovers.

## Configuration (environment)

| Variable | Default | |
|---|---|---|
| `DATA_DIR` | `/data` | settings, state, keystore, cache |
| `INTERVAL` | `6h` | time between scheduled runs |
| `WEB_PORT` / `WEB_BIND` | `8093` / `0.0.0.0` | |
| `ALLOWED_HOST_SUFFIXES` | | extra `Host` names the page answers to |
| `FLARESOLVERR_URL` | | e.g. `http://127.0.0.1:8191` |
| `APKMIRROR_MIN_INTERVAL` | `4s` | minimum gap between APKMirror requests |
| `APKMIRROR_COOLDOWN` | `5m` | first pause after APKMirror pushes back (doubles, up to 1h) |
| `PATCHES_REPO` / `PATCHES_PRERELEASE` | `ajstrick81/morphe-androidtv-patches` / `false` | seeds the patch sources on the first run only |
| `PATCH_SOURCES_ALLOWED_OWNERS` | | limit repositories added from the page to these GitHub owners |
| `BYTECODE_MODE` | `STRIP_SAFE` | Morphe `--bytecode-mode` |
| `FETCH_FAILURES_BEFORE_ALERT` | `3` | |
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
