# TV Patcher

Keeps Morphe-patched apps (Prime Video, Netflix, Disney+, and anything else the
[morphe-androidtv-patches](https://github.com/ajstrick81/morphe-androidtv-patches) support) installed
on your Google TVs. A small web page lets you add devices and tick apps; a scheduler does the rest:

1. find the newest patches release and the newest app version it supports;
2. get that exact Android TV bundle (your drop folder, the cache, APKMirror, APKMirror through
   FlareSolverr, APKPure) and verify it;
3. patch it once with the Morphe CLI for the TV's CPU;
4. install it over wireless ADB on every device that needs it, and tell you on Telegram.

Written in Go; the image adds the Java runtime, `adb`, `apksigner` and `apkeep`.

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

## Where APKs come from

In order, the first file that passes every check wins:

1. your drop folder `/data/work/input/<package>/` (any `.apkm`, `.xapk`, `.apk`);
2. the cache;
3. APKMirror, directly, rotating browser profiles with backoff when rate limited;
4. APKMirror through a FlareSolverr sidecar (`FLARESOLVERR_URL`);
5. APKPure through `apkeep`, only if you set `APKPURE_ACKNOWLEDGE_DANGERS=true`.

The APKMirror page for an app is found automatically (search by package name, an Android TV page
first) and cached; `settings.json` can override a wrong guess. Nothing is downgraded: if the newest
supported version cannot be found you are told, with the exact file to drop in.

**Every file is verified:** right package and version, an Android TV launcher (rejects phone builds),
`armeabi-v7a`, and the publisher's signing certificate: learned from the first APKMirror download,
or pinned with `signer_sha256`; `apksigner` checks it cryptographically. APKPure files are refused
until a signer is on record, because apkeep's docs warn APKPure has modified apps to spread malware.

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
| `APKPURE_ACKNOWLEDGE_DANGERS` | `false` | allow APKPure (signer-checked) |
| `PATCHES_REPO` / `PATCHES_PRERELEASE` | `ajstrick81/morphe-androidtv-patches` / `false` | |
| `BYTECODE_MODE` | `STRIP_SAFE` | Morphe `--bytecode-mode` |
| `FETCH_FAILURES_BEFORE_ALERT` | `3` | |
| `TELEGRAM_BOT_TOKEN` / `TELEGRAM_CHAT_ID` | | notifications |
| `GITHUB_TOKEN` | | optional, avoids API rate limits |

`/data/state/settings.json` holds your devices and apps (edited by the page) plus optional per-app
`overrides`: `apkmirror` (publisher/app path), `sources`, `signer_sha256`, `enable`/`disable` (Morphe
patch names), `options_file`, `allow_uninstall`.

**Back up the signing keystore** (`/data/keystore/morphe.keystore`, in the data volume). Lose it and
every app has to be uninstalled before it can update.

## Commands

```
tv-patcher run                  web page + scheduler (default)
tv-patcher once [--device ID] [--app PKG,PKG] [--force] [--dry-run] [--no-device]
tv-patcher pair --device ID --pair-port N --code 123456
tv-patcher fetch --package PKG [--version V]     download and verify one app
tv-patcher resolve [--package PKG]               find apps on APKMirror, no download
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
up new Morphe and apkeep releases); it needs the repo secret `WORKFLOW_TOKEN`. `live-checks.yml` is
a manual workflow that proves APKPure, FlareSolverr and app discovery work from a runner.
