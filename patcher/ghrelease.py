import json
from pathlib import Path

import requests

from .util import log, retry


def _headers(token):
    h = {"Accept": "application/vnd.github+json"}
    if token:
        h["Authorization"] = f"Bearer {token}"
    return h


def latest_release(repo: str, token: str = "", prerelease: bool = False) -> dict:
    """Return newest release (by published date) carrying a .mpp asset."""
    def go():
        r = requests.get(f"https://api.github.com/repos/{repo}/releases?per_page=10",
                         headers=_headers(token), timeout=30)
        r.raise_for_status()
        return r.json()
    rels = retry(go, what="github releases")
    for rel in rels:
        if rel.get("draft") or (rel.get("prerelease") and not prerelease):
            continue
        if any(a["name"].endswith(".mpp") for a in rel["assets"]):
            return rel
    raise RuntimeError(f"no release with .mpp asset in {repo}")


def download_mpp(rel: dict, cache: Path, token: str = "") -> Path:
    asset = next(a for a in rel["assets"] if a["name"].endswith(".mpp"))
    dest = cache / asset["name"]
    if dest.exists() and dest.stat().st_size == asset["size"]:
        return dest
    log.info("downloading %s", asset["name"])

    def go():
        with requests.get(asset["browser_download_url"], stream=True, timeout=60) as r:
            r.raise_for_status()
            tmp = dest.with_suffix(".part")
            with open(tmp, "wb") as f:
                for chunk in r.iter_content(1 << 20):
                    f.write(chunk)
            tmp.replace(dest)
    retry(go, what="download mpp")
    # drop older bundles
    for old in cache.glob("patches-*.mpp"):
        if old != dest:
            old.unlink()
    return dest


def patches_list_json(repo: str, tag: str, token: str = "") -> dict:
    url = f"https://raw.githubusercontent.com/{repo}/{tag}/patches-list.json"
    r = retry(lambda: requests.get(url, headers=_headers(token), timeout=30), what="patches-list.json")
    r.raise_for_status()
    return json.loads(r.text)
