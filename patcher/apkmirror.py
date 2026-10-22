"""Best-effort APKMirror scraper. Cloudflare may block it; callers fall back to the drop folder."""
import re
import time
from pathlib import Path
from urllib.parse import urljoin

from bs4 import BeautifulSoup
from curl_cffi import requests as cr

from .util import log, retry

BASE = "https://www.apkmirror.com"
ARCH_OK = ("armeabi-v7a", "universal", "noarch")


def version_slug(v: str) -> str:
    s = v.lower().replace("+", "")
    s = re.sub(r"[^a-z0-9]+", "-", s)
    return s.strip("-")


def _get(sess, url, **kw):
    time.sleep(1.5)
    r = sess.get(url, timeout=60, **kw)
    return r


def _soup(sess, url):
    r = _get(sess, url)
    if r.status_code != 200:
        raise RuntimeError(f"{url} -> HTTP {r.status_code}")
    return BeautifulSoup(r.text, "html.parser"), r


def release_url(sess, slug: str, version: str) -> str:
    """Guess the release URL, else look through the app's release list."""
    tail = slug.rstrip("/").split("/")[-1]
    vs = version_slug(version)
    guess = f"{BASE}/apk/{slug}/{tail}-{vs}-release/"
    r = _get(sess, guess)
    if r.status_code == 200:
        return guess
    for page in range(1, 4):
        soup, _ = _soup(sess, f"{BASE}/uploads/page/{page}/?appcategory={tail}")
        for a in soup.select("a[href*='-release/']"):
            h = a["href"]
            if h.rstrip("/").endswith(f"{vs}-release") and f"/{slug}/" in h:
                return urljoin(BASE, h)
    raise LookupError(f"{version} not found on APKMirror for {slug}")


def pick_variant(soup, device_sdk: int) -> str:
    """Return variant page URL: BUNDLE row, compatible arch, minSdk <= device."""
    for row in soup.select("div.table-row.headerFont"):
        text = row.get_text(" ", strip=True)
        if "BUNDLE" not in text.upper():
            continue
        link = row.select_one("a.accent_color")
        if not link:
            continue
        low = text.lower()
        if not any(a in low for a in ARCH_OK):
            continue
        m = re.search(r"android\s*(\d+)", low)  # "Android 5.0+" style; loose check only
        if m and int(m.group(1)) > 16:
            continue
        return urljoin(BASE, link["href"])
    raise LookupError("no compatible BUNDLE variant on release page")


def download(slug: str, version: str, device_sdk: int, dest_dir: Path) -> Path:
    sess = cr.Session(impersonate="chrome")
    rel = retry(lambda: release_url(sess, slug, version), what="apkmirror release")
    log.info("release page %s", rel)
    soup, _ = _soup(sess, rel)
    variant = pick_variant(soup, device_sdk)
    soup, _ = _soup(sess, variant)
    btn = soup.select_one("a.downloadButton[href*='/download/']") or soup.select_one("a.downloadButton")
    if not btn:
        raise LookupError("download button missing")
    soup, _ = _soup(sess, urljoin(BASE, btn["href"]))
    final = soup.select_one("a[href*='download.php']") or soup.select_one("a[rel='nofollow'][href*='key=']")
    if not final:
        raise LookupError("final download link missing (blocked?)")
    url = urljoin(BASE, final["href"])
    dest = dest_dir / f"{slug.split('/')[-1]}-{version_slug(version)}.apkm"
    tmp = dest.with_suffix(".part")
    time.sleep(2)
    r = sess.get(url, timeout=600, stream=True)
    if r.status_code != 200:
        raise RuntimeError(f"download HTTP {r.status_code}")
    with open(tmp, "wb") as f:
        for chunk in r.iter_content(1 << 20):
            f.write(chunk)
    tmp.replace(dest)
    return dest
