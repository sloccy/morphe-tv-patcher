"""Find a verified APK bundle for (app, version): drop folder, cache, APKMirror, FlareSolverr, APKPure."""
from dataclasses import dataclass, field
from pathlib import Path

from . import apkmirror, apkpure
from .errors import Blocked, NotFound, VerifyError
from .util import log
from .verify import BUNDLE_EXTS, Verified, verify


@dataclass
class Fetched:
    verified: Verified
    detail: str          # e.g. "apkmirror (chrome)", "apkpure", "dropfolder"
    learned_signer: bool # first time we recorded this app's signer


class FetchError(RuntimeError):
    def __init__(self, attempts, fatal):
        super().__init__("; ".join(attempts) or "no source available")
        self.attempts, self.fatal = attempts, fatal


def _local(dirs, app, version, pinned, label, attempts, fatal_msgs):
    for d in dirs:
        if not d.is_dir():
            continue
        for f in sorted(d.iterdir()):
            if f.suffix not in BUNDLE_EXTS:
                continue
            try:
                return verify(f, app.package, version, label, pinned)
            except VerifyError as e:
                if e.kind == "version" or (label == "cache" and e.kind in ("unpinned", "package")):
                    continue  # an old file, or another app's file in the shared cache
                msg = f"{label} {f.name}: {e}"
                (fatal_msgs if e.fatal else attempts).append(msg)
                log.warning(msg)
            except Exception as e:  # noqa: BLE001
                attempts.append(f"{label} {f.name}: {type(e).__name__}: {e}")
    return None


def fetch(cfg, app, version, device_sdk, state) -> Fetched:
    pinned = app.signer_sha256 or state.get(app.package).get("signer_sha256", "")
    attempts, fatal_msgs = [], []
    mirror = state.get("_apkmirror")

    def done(v: Verified, detail):
        learned = not pinned
        if learned:
            state.set(app.package, signer_sha256=v.signer)
            log.warning("recorded signer %s for %s (pin it as signer_sha256 to lock it in)",
                        v.signer, app.package)
        return Fetched(v, detail, learned)

    v = _local([cfg.inputs / app.package], app, version, pinned, "dropfolder", attempts, fatal_msgs)
    if v:
        if not pinned:
            log.warning("%s: manual file accepted with no pinned signer", app.package)
        return done(v, "dropfolder")
    v = _local([cfg.cache], app, version, pinned, "cache", attempts, fatal_msgs)
    if v:
        return done(v, "cache")

    def try_download(label, fn):
        try:
            path, extra = fn()
        except (Blocked, NotFound) as e:
            attempts.append(f"{label}: {type(e).__name__}: {e}")
            log.warning("%s: %s", label, e)
            return None
        except Exception as e:  # noqa: BLE001
            attempts.append(f"{label}: {type(e).__name__}: {e}")
            log.warning("%s failed: %s", label, e)
            return None
        try:
            return verify(path, app.package, version, label.split()[0], pinned), extra
        except VerifyError as e:
            path.unlink(missing_ok=True)
            (fatal_msgs if e.fatal else attempts).append(f"{label}: {e}")
            log.warning("%s rejected: %s", label, e)
            return None

    for src in app.sources:
        if src == "apkmirror" and cfg.apkmirror_enabled:
            if cfg.apkmirror_direct:
                r = try_download("apkmirror", lambda: apkmirror.download(
                    app.apkmirror, version, device_sdk, cfg.cache,
                    profile_hint=mirror.get("last_profile", "")))
                if r:
                    state.set("_apkmirror", last_profile=r[1])
                    return done(r[0], f"apkmirror ({r[1]})")
            if cfg.flaresolverr_url:
                r = try_download("apkmirror via flaresolverr", lambda: apkmirror.download(
                    app.apkmirror, version, device_sdk, cfg.cache, flare_url=cfg.flaresolverr_url))
                if r:
                    return done(r[0], "apkmirror (flaresolverr)")
        elif src == "apkpure":
            if not apkpure.available():
                attempts.append("apkpure: apkeep not installed")
                continue
            r = try_download("apkpure", lambda: (apkpure.download(app.package, version, cfg.cache), ""))
            if r:
                return done(r[0], "apkpure")
    raise FetchError(attempts + fatal_msgs, fatal=bool(fatal_msgs))
