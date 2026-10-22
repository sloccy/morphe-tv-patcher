import json
import zipfile
from pathlib import Path


def read_info(path: Path) -> dict:
    """Return {package, version, ...} from an .apkm; raises if unreadable."""
    with zipfile.ZipFile(path) as z:
        info = json.loads(z.read("info.json"))
    return parse_info(info)


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
