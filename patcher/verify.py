"""One set of checks every candidate APK bundle must pass, whatever its source."""
import json
import logging
import re
import shutil
import subprocess
import tempfile
import zipfile
from dataclasses import dataclass
from pathlib import Path

from .errors import VerifyError
from .util import log

logging.getLogger("androguard").setLevel(logging.CRITICAL)
try:  # androguard logs through loguru at INFO by default
    from loguru import logger as _loguru
    _loguru.remove()
except Exception:  # noqa: BLE001
    pass

BUNDLE_EXTS = (".apkm", ".xapk", ".apks", ".apk")
ABI_SPLIT = re.compile(r"(?:split_)?config\.(armeabi_v7a|arm64_v8a|x86_64|x86)\.apk$")
SPLIT_NAME = re.compile(r"^(?:split_)?config\.")
TARGET_ABI = "armeabi-v7a"


@dataclass
class Verified:
    path: Path
    source: str
    signer: str
    package: str
    version: str


def _norm_abi(a: str) -> str:
    return a.replace("_", "-")


def pick_base(z, cands) -> str:
    """APKMirror: base.apk. APKPure XAPK: <package_name>.apk (splits are config.*.apk)."""
    if "base.apk" in cands:
        return "base.apk"
    if "manifest.json" in z.namelist():
        try:
            pkg = json.loads(z.read("manifest.json")).get("package_name")
        except Exception:  # noqa: BLE001
            pkg = None
        if pkg and f"{pkg}.apk" in cands:
            return f"{pkg}.apk"
    rest = [n for n in cands if not SPLIT_NAME.match(n)]
    return max(rest, key=lambda n: z.getinfo(n).file_size) if rest else None


def _extract_base(path: Path, tmp: Path) -> tuple:
    """Return (base_apk_path, set_of_native_abis)."""
    if not zipfile.is_zipfile(path):
        raise VerifyError("format", "not a zip/apk")
    abis = set()
    with zipfile.ZipFile(path) as z:
        names = z.namelist()
        if path.suffix == ".apk":
            base = path
            members = None
        else:
            cands = [n for n in names if "/" not in n and n.endswith(".apk")]
            base_name = pick_base(z, cands)
            if not base_name:
                raise VerifyError("format", "no base apk inside bundle")
            z.extract(base_name, tmp)
            base = tmp / base_name
            for n in cands:
                m = ABI_SPLIT.search(n)
                if m:
                    abis.add(_norm_abi(m.group(1)))
    with zipfile.ZipFile(base) as z:
        for n in z.namelist():
            m = re.match(r"lib/([^/]+)/", n)
            if m:
                abis.add(m.group(1))
    return base, abis


def inspect(path: Path) -> dict:
    """Facts about a bundle, read from the real manifest (not the container's label)."""
    from androguard.core.apk import APK
    with tempfile.TemporaryDirectory() as t:
        base, abis = _extract_base(path, Path(t))
        try:
            a = APK(str(base))
        except Exception as e:  # noqa: BLE001
            raise VerifyError("format", f"unreadable manifest: {e}")
        certs = a.get_certificates()
        signer = certs[0].sha256_fingerprint.replace(" ", "").replace(":", "").lower() if certs else ""
        apksigner_ok, apksigner_msg = _apksigner(base)
        return {"package": a.get_package(), "version": a.get_androidversion_name(),
                "leanback": bool(a.is_leanback()), "abis": abis, "signer": signer,
                "apksigner_ok": apksigner_ok, "apksigner_msg": apksigner_msg}


_warned = False


def _apksigner(base: Path):
    """Cryptographic verification when apksigner exists (it does in the image). (True|None, msg)."""
    global _warned
    exe = shutil.which("apksigner")
    if not exe:
        if not _warned:
            log.warning("apksigner not found; skipping cryptographic signature check")
            _warned = True
        return None, ""
    r = subprocess.run([exe, "verify", str(base)], capture_output=True, text=True, timeout=300)
    return r.returncode == 0, (r.stdout + r.stderr).strip()[:300]


def verify(path: Path, package: str, version: str, source: str,
           pinned: str = "") -> Verified:
    """Raise VerifyError, or return Verified. `pinned` is the known signer sha256 (may be empty).

    A file with no pinned signer is only acceptable from APKMirror (trusted for the first
    record) or the drop folder (you put it there yourself)."""
    i = inspect(path)
    if i["package"] != package:
        raise VerifyError("package", f"{i['package']} != {package}")
    if i["version"] != version:
        raise VerifyError("version", f"{i['version']} != {version}")
    if not i["leanback"]:
        raise VerifyError("leanback", "no Android TV launcher activity (phone build?)")
    if i["abis"] and TARGET_ABI not in i["abis"]:
        raise VerifyError("arch", f"native code for {sorted(i['abis'])}, need {TARGET_ABI}")
    if not i["signer"]:
        raise VerifyError("signature", "no signing certificate found")
    if i["apksigner_ok"] is False:
        raise VerifyError("signature", f"apksigner verify failed: {i['apksigner_msg']}")
    if pinned and i["signer"] != pinned:
        raise VerifyError("signature", f"signer {i['signer'][:16]}… != pinned {pinned[:16]}…")
    if not pinned and source not in ("apkmirror", "dropfolder"):
        raise VerifyError("unpinned", f"no signer on record yet; only APKMirror or a manual drop can set it ({source})")
    return Verified(path, source, i["signer"], i["package"], i["version"])
