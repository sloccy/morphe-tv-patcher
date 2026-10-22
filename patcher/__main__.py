import argparse
import logging
import sys
import time
from dataclasses import dataclass

from . import config, ghrelease, morphe, notify, sources, versions
from .adb import Adb
from .sources import FetchError
from .state import State
from .util import log

DAY = 86400


@dataclass
class Result:
    ok: bool
    msg: str
    alert: bool = False     # send a notification for this
    changed: bool = False   # something was installed / patched


def _prune(directory, patterns, keep):
    files = sorted((f for pat in patterns for f in directory.glob(pat) if f.is_file()),
                   key=lambda f: f.stat().st_mtime, reverse=True)
    for old in files[keep:]:
        old.unlink(missing_ok=True)


def fetch_failed(cfg, app, state, target, err: FetchError, now=None) -> Result:
    """Temporary problems wait for N failed cycles; wrong/tampered builds alert at once.
    Either way at most one alert per 24 h per app."""
    now = now or time.time()
    st = state.get(app.package)
    n = st.get("fetch_failures", 0) + 1
    cooled = now - st.get("last_fetch_alert", 0) > DAY
    alert = cooled and (err.fatal or n >= cfg.fetch_failures_before_alert)
    state.set(app.package, fetch_failures=n, last_fetch_error=str(err)[:500],
              **({"last_fetch_alert": now} if alert else {}))
    msg = (f"{app.package}: cannot get {target} ({n} cycle{'s' if n != 1 else ''}): {err}. "
           f"Drop the .apkm into {cfg.inputs / app.package}/ (newest supported version; "
           f"no fallback to older)")
    return Result(ok=False, msg=msg, alert=alert)


def process(cfg, app, adb, mpp, rel, patches_list, state, args) -> Result:
    vs = versions.supported(cfg.morphe_jar, mpp, app.package, patches_list)
    if not vs:
        return Result(False, f"{app.package}: no supported versions in bundle", alert=True)
    target = versions.newest(vs)
    patches_ver = rel["tag_name"]
    prev = state.get(app.package)
    installed = adb.installed_version(app.installed_package) if adb else None
    if (not args.force and prev.get("apk_version") == target
            and prev.get("patches_version") == patches_ver and installed):
        return Result(True, f"{app.package}: up to date ({target}, patches {patches_ver})")

    try:
        fetched = sources.fetch(cfg, app, target, adb.sdk() if adb else 28, state)
    except FetchError as e:
        return fetch_failed(cfg, app, state, target, e)

    out = cfg.output / f"{app.package}-{target.replace(' ', '_')}-{patches_ver}.apk"
    morphe.patch(cfg, app, mpp, fetched.verified.path, out, adb.abis() if adb else [], patches_ver)
    via = f"via {fetched.detail}"
    if args.dry_run or not adb:
        return Result(True, f"{app.package}: patched {target} {via} (dry run, not installed) -> {out.name}",
                      changed=True)

    ok, msg = adb.install(out)
    if not ok:
        if "UPDATE_INCOMPATIBLE" in msg and app.allow_uninstall and app.package != "com.netflix.ninja":
            adb._run("-s", adb.serial, "uninstall", app.installed_package)
            ok, msg = adb.install(out)
        elif "UPDATE_INCOMPATIBLE" in msg:
            raise RuntimeError(f"signature mismatch with installed app. Run: adb uninstall "
                               f"{app.installed_package} (loses login) or set allow_uninstall: true")
    if not ok:
        raise RuntimeError(f"install failed: {msg[-300:]}")

    recovered = bool(prev.get("last_fetch_alert"))
    state.set(app.package, apk_version=target, patches_version=patches_ver, source=fetched.detail,
              installed_at=time.strftime("%Y-%m-%dT%H:%M:%S"),
              fetch_failures=None, last_fetch_error=None, last_fetch_alert=None)
    _prune(cfg.output, [f"{app.package}-*.apk", f"{app.package}-*.result.json"], keep=4)
    _prune(cfg.cache, ["*.apkm", "*.xapk", "*.apks"], keep=6)
    text = f"{app.package}: {prev.get('apk_version', installed or 'none')} -> {target} (patches {patches_ver}) {via}"
    return Result(True, ("recovered, " if recovered else "") + text, alert=True, changed=True)


def cycle(cfg, args) -> int:
    alerts, failed = [], False
    rel = ghrelease.latest_release(cfg.patches_repo, cfg.github_token, cfg.prerelease)
    mpp = ghrelease.download_mpp(rel, cfg.cache, cfg.github_token)
    try:
        patches_list = ghrelease.patches_list_json(cfg.patches_repo, rel["tag_name"], cfg.github_token)
    except Exception as e:  # noqa: BLE001
        log.warning("patches-list.json unavailable: %s", e)
        patches_list = None
    adb = None
    if not args.no_device:
        adb = Adb(cfg.host, cfg.port, cfg.data / "adb_port")
        adb.connect()
    state = State(cfg.state_file)
    for app in cfg.apps:
        if args.app and app.package != args.app:
            continue
        try:
            res = process(cfg, app, adb, mpp, rel, patches_list, state, args)
        except Exception as e:  # noqa: BLE001
            log.exception("%s failed", app.package)
            res = Result(False, f"{app.package}: {e}", alert=True)
        log.info(res.msg)
        failed |= not res.ok
        if res.alert:
            alerts.append(res.msg)
    if alerts:
        notify.send("Morphe patcher: " + ("PROBLEMS" if failed else "updated"), "\n".join(alerts))
    return 1 if failed else 0


def main():
    ap = argparse.ArgumentParser(prog="patcher")
    ap.add_argument("cmd", choices=["run", "once", "pair"], nargs="?", default="run")
    ap.add_argument("--config", default="/config/config.yaml")
    ap.add_argument("--dry-run", action="store_true", help="patch but do not install")
    ap.add_argument("--no-device", action="store_true", help="skip adb entirely (implies dry run)")
    ap.add_argument("--pair-port", type=int, help="pair: port shown in the TV pairing dialog")
    ap.add_argument("--code", help="pair: 6-digit pairing code")
    ap.add_argument("--app", help="only this package")
    ap.add_argument("--force", action="store_true")
    args = ap.parse_args()
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    cfg = config.load(args.config)
    if args.cmd == "pair":
        if not (args.pair_port and args.code):
            sys.exit("usage: pair --pair-port PORT --code 123456")
        print(Adb(cfg.host).pair(args.pair_port, args.code))
        return
    if args.cmd == "once":
        sys.exit(cycle(cfg, args))
    while True:
        try:
            cycle(cfg, args)
        except Exception as e:  # noqa: BLE001
            log.exception("cycle failed")
            notify.send("Morphe patcher: cycle failed", str(e))
        time.sleep(cfg.interval_s)


if __name__ == "__main__":
    main()
