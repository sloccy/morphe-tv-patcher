import argparse
import logging
import sys
import time

from . import apkm, apkmirror, config, ghrelease, morphe, notify, versions
from .adb import Adb
from .state import State
from .util import log


def find_apkm(cfg, app, version):
    """Drop folder first, then cache."""
    for d in (cfg.inputs / app.package, cfg.cache):
        if d.is_dir():
            for f in sorted(d.glob("*.apkm")):
                if apkm.matches(f, app.package, version):
                    return f
    return None


def process(cfg, app, adb, mpp, rel, patches_list, state, args) -> str:
    """Returns a status string; raises on failure."""
    vs = versions.supported(cfg.morphe_jar, mpp, app.package, patches_list)
    if not vs:
        return f"{app.package}: no supported versions in bundle"
    target = versions.newest(vs)
    patches_ver = rel["tag_name"]
    prev = state.get(app.package)
    installed = adb.installed_version(app.installed_package) if adb else None
    # installed versionName can't be compared 1:1 with the clone/patched apk, so state is primary
    if (not args.force and prev.get("apk_version") == target
            and prev.get("patches_version") == patches_ver and installed):
        return f"{app.package}: up to date ({target}, patches {patches_ver})"

    src = find_apkm(cfg, app, target)
    if not src and cfg.apkmirror_enabled:
        try:
            sdk = adb.sdk() if adb else 28
            src = apkmirror.download(app.apkmirror, target, sdk, cfg.cache)
            if not apkm.matches(src, app.package, target):
                src.unlink(missing_ok=True)
                raise RuntimeError("downloaded file does not match package/version")
        except Exception as e:  # noqa: BLE001
            log.warning("apkmirror failed for %s %s: %s", app.package, target, e)
    if not src:
        return (f"{app.package}: need {target}. Drop the .apkm into "
                f"{cfg.inputs / app.package}/ (newest supported version; not falling back)")

    out = cfg.output / f"{app.package}-{target.replace(' ', '_')}-{patches_ver}.apk"
    abis = adb.abis() if adb else []
    morphe.patch(cfg, app, mpp, src, out, abis, patches_ver)
    if args.dry_run or not adb:
        return f"{app.package}: patched {target} (dry run, not installed) -> {out.name}"

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
    state.set(app.package, apk_version=target, patches_version=patches_ver,
              installed_at=time.strftime("%Y-%m-%dT%H:%M:%S"))
    return f"{app.package}: {prev.get('apk_version', installed or 'none')} -> {target} (patches {patches_ver})"


def cycle(cfg, args) -> int:
    failures, updates = [], []
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
            msg = process(cfg, app, adb, mpp, rel, patches_list, state, args)
            log.info(msg)
            if "->" in msg or "need " in msg:
                updates.append(msg)
        except Exception as e:  # noqa: BLE001
            log.exception("%s failed", app.package)
            failures.append(f"{app.package}: {e}")
    if failures:
        notify.send("Morphe patcher: FAILURES", "\n".join(failures + updates))
    elif updates:
        notify.send("Morphe patcher", "\n".join(updates))
    return 1 if failures else 0


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
