import re
import subprocess

from .util import log

_NUM = re.compile(r"(\d+)|([^\d]+)")


def natural_key(v: str):
    """Numeric runs compare as numbers, so 7.10.102 > 7.8.100."""
    key = []
    for num, txt in _NUM.findall(v):
        key.append((1, int(num), "") if num else (0, 0, txt))
    return key


def newest(versions) -> str:
    vs = sorted(set(versions), key=natural_key)
    if not vs:
        raise ValueError("no versions")
    return vs[-1]


def from_patches_list(data: dict, package: str) -> list:
    out = set()
    for p in data.get("patches", []):
        out.update((p.get("compatiblePackages") or {}).get(package) or [])
    return sorted(out, key=natural_key)


def from_list_versions(jar: str, mpp, package: str) -> list:
    """Best-effort parse of `list-versions`. Empty list on any problem."""
    try:
        res = subprocess.run(["java", "-jar", jar, "list-versions", "--patches", str(mpp),
                              "-f", package], capture_output=True, text=True, timeout=180)
    except Exception as e:  # noqa: BLE001
        log.warning("list-versions failed: %s", e)
        return []
    if res.returncode != 0:
        return []
    out = []
    for line in res.stdout.splitlines():
        m = re.match(r"^\s*[-*]?\s*([0-9][^\s(]*(?: build \d+)?)\s*(?:\(.*\))?\s*$", line)
        if m:
            out.append(m.group(1))
    return out


def supported(jar, mpp, package, fallback_data) -> list:
    """Union of the CLI's view and patches-list.json; either may be empty."""
    vs = set(from_list_versions(jar, mpp, package))
    if fallback_data:
        vs |= set(from_patches_list(fallback_data, package))
    return sorted(vs, key=natural_key)
