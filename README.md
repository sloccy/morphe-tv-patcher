# morphe-tv-patcher

Docker service: checks for new `ajstrick81/morphe-androidtv-patches` releases, downloads the newest
supported stock Android TV `.apkm` (APKMirror, or your drop folder), patches with Morphe CLI, and
installs over network ADB to the onn 4K Pro. Notifies on Telegram.

## Setup
1. onn box: Settings > System > Developer options > enable **Wireless debugging**; give the box a static DHCP lease.
2. `cp config.example.yaml config/config.yaml`, set `device.host` (IP only). Google TV assigns a random port that
   changes on toggle/reboot; the patcher finds it (saved port, mDNS, then a 30000-50000 port scan).
   Pair once: on the TV choose "Pair device with pairing code", then
   `docker compose run --rm patcher pair --pair-port <port on TV> --code <6 digits>`.
3. `docker compose up -d` (image: `ghcr.io/sloccy/morphe-tv-patcher`).
4. Pairing keys persist in `the `adbkeys` volume`, so pair only once. If Wireless debugging switches off after a reboot,
   turn it back on; no re-pair needed.
5. **Back up `/data/keystore/morphe.keystore` (the `state` volume).** Losing it means uninstalling apps (logins lost) to update.
   Apps previously installed via Morphe Manager use Manager's key; expect a one-time uninstall/reinstall
   (the patcher reports `INSTALL_FAILED_UPDATE_INCOMPATIBLE` instead of uninstalling; never touches stock Netflix).

## Notifications
Uses the homelab Telegram bot: the compose file loads `/srv/secrets/telegram.env`
(`TELEGRAM_BOT_TOKEN`, `TELEGRAM_CHAT_ID`), the same file Gatus reads. Unset = log only.

## Commands
`docker compose run --rm patcher once --dry-run` patches without installing (still needs the TV for ABI/SDK; add `--no-device` to skip).
`--app <package>`, `--force` supported. If APKMirror blocks scraping, drop the `.apkm` in `/data/work/input/<package>/` (the `work` volume); the log/notification names the exact version.

## Policy
Always the newest app version the current patch bundle supports. No pinning, no silent fallback to older.

## CI
`bump-morphe.yml` (daily) bumps `morphe-cli.version` to the latest MorpheApp/morphe-cli release and triggers
`build.yml`, which tests and pushes a multi-arch image to GHCR.
