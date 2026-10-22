import json
import zipfile
from pathlib import Path


def read_info(path: Path) -> dict:
    """Return {package, version, ...} from an .apkm; raises if unreadable."""
    with zipfile.ZipFile(path) as z:
        names = set(z.namelist())
        if "info.json" in names:
            return parse_info(json.loads(z.read("info.json")))
        if "manifest.json" in names:
            return parse_xapk_manifest(json.loads(z.read("manifest.json")))
    raise ValueError("no info.json or manifest.json")


def parse_xapk_manifest(m: dict) -> dict:
    return {
        "package": m.get("package_name"),
        "version": m.get("version_name"),
        "code": m.get("version_code"),
        "min_sdk": m.get("min_sdk_version"),
        "arches": m.get("native_code") or [],
    }


def parse_info(info: dict) -> dict:
    return {
        "package": info.get("pname"),
        # APKMirror bundles use release_version; versionname is kept for other sources
        "version": info.get("release_version") or info.get("versionname"),
        "code": info.get("versioncode"),
        "min_sdk": info.get("min_api"),
        "arches": info.get("arches") or [],
    }


def matches(path: Path, package: str, version: str) -> bool:
    try:
        i = read_info(path)
    except Exception:  # noqa: BLE001
        return False
    return i["package"] == package and i["version"] == version
