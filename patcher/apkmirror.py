"""APKMirror scraper: direct (curl_cffi, rotating browser profiles) or through FlareSolverr."""
import random
import re
import time
from pathlib import Path
from urllib.parse import urljoin

from bs4 import BeautifulSoup
from curl_cffi import requests as cr

from . import flaresolverr
from .errors import Blocked, NotFound
from .util import log

BASE = "https://www.apkmirror.com"
ARCH_OK = ("armeabi-v7a", "universal", "noarch")
PROFILES = ("chrome", "chrome_android", "safari", "firefox")
BACKOFF_S = (30, 90)
BLOCK_CODES = (403, 429, 503)


def _sleep(a=2.0, b=6.0):
    time.sleep(random.uniform(a, b))


def version_slug(v: str) -> str:
    s = v.lower().replace("+", "")
    return re.sub(r"[^a-z0-9]+", "-", s).strip("-")


class Client:
    """Fetch pages. mode 'direct' uses curl_cffi; 'flare' loads pages in FlareSolverr's browser
    and reuses its cookies + user agent for the file download."""

    def __init__(self, profile="chrome", flare_url=""):
        self.profile, self.flare_url = profile, flare_url
        self.cookies, self.ua = {}, None
        self.sess = cr.Session(impersonate=profile)
        self.last = None

    def html(self, url, referer=None) -> BeautifulSoup:
        if self.flare_url:
            sol = flaresolverr.solve(self.flare_url, url)
            self.cookies.update(sol["cookies"])
            self.ua = sol["user_agent"] or self.ua
            status, text = sol["status"], sol["html"]
        else:
            r = self.sess.get(url, timeout=60, headers={"Referer": referer} if referer else None)
            status, text = r.status_code, r.text
        self.last = url
        if status in BLOCK_CODES or "Just a moment..." in text[:2000]:
            raise Blocked(f"{url} -> HTTP {status} (Cloudflare)")
        if status == 404:
            raise NotFound(url)
        if status != 200:
            raise RuntimeError(f"{url} -> HTTP {status}")
        soup = BeautifulSoup(text, "html.parser")
        # through a real browser the 404 page can arrive as a 200
        if soup.title and "Page Not Found" in soup.title.get_text():
            raise NotFound(url)
        return soup

    def file(self, url, referer, dest: Path):
        if self.flare_url:
            # the file URL can have its own Cloudflare challenge; let the browser solve it
            # first so we hold the clearance cookie for the same IP + user agent
            try:
                sol = flaresolverr.solve(self.flare_url, url)
                self.cookies.update(sol["cookies"])
                self.ua = sol["user_agent"] or self.ua
                log.info("flaresolverr visited file url; cookies: %s", sorted(sol["cookies"]))
            except Blocked as e:
                log.info("flaresolverr could not open the file url: %s", e)
        headers = {"Referer": referer}
        if self.ua:
            headers["User-Agent"] = self.ua
        r = self.sess.get(url, timeout=900, stream=True, headers=headers, cookies=self.cookies or None)
        if r.status_code in BLOCK_CODES:
            raise Blocked(f"download HTTP {r.status_code}")
        if r.status_code != 200:
            raise RuntimeError(f"download HTTP {r.status_code}")
        tmp = dest.with_suffix(".part")
        with open(tmp, "wb") as f:
            for chunk in r.iter_content(1 << 20):
                f.write(chunk)
        with open(tmp, "rb") as f:
            if f.read(2) != b"PK":  # HTML interstitial instead of the bundle
                tmp.unlink()
                raise Blocked("download was not a zip (interstitial page?)")
        tmp.replace(dest)


def find_release(c: Client, slug: str, version: str) -> tuple:
    """Guess the release URL; otherwise look the version up on the app's own page."""
    tail = slug.rstrip("/").split("/")[-1]
    vs = version_slug(version)
    guess = f"{BASE}/apk/{slug}/{tail}-{vs}-release/"
    try:
        return guess, c.html(guess, referer=BASE + "/")
    except NotFound:
        pass
    for page in (1, 2, 3):
        _sleep()
        url = f"{BASE}/apk/{slug}/" + (f"page/{page}/" if page > 1 else "")
        soup = c.html(url, referer=BASE + "/")
        for a in soup.select("a[href*='-release/']"):
            h = a["href"].split("#")[0]
            if h.rstrip("/").endswith(f"{vs}-release") and f"/{slug}/" in h:
                url = urljoin(BASE, h)
                _sleep()
                return url, c.html(url, referer=BASE + "/")
    raise NotFound(f"{version} not on APKMirror for {slug}")


def pick_variant(soup, device_sdk: int) -> str:
    """Variant page URL: a BUNDLE row with a compatible arch and minSdk <= device."""
    for row in soup.select("div.table-row.headerFont"):
        text = row.get_text(" ", strip=True)
        if "BUNDLE" not in text.upper():
            continue
        link = row.select_one("a.accent_color")
        low = text.lower()
        if not link or not any(a in low for a in ARCH_OK):
            continue
        m = re.search(r"android\s*(\d+)", low)
        # "Android 7.0+" is the minimum API level's marketing version; be loose, only reject the absurd
        if m and int(m.group(1)) > 16:
            continue
        return urljoin(BASE, link["href"])
    title = soup.title.get_text(strip=True)[:80] if soup.title else "no <title>"
    rows = [r.get_text(" ", strip=True)[:70] for r in soup.select("div.table-row.headerFont")][:3]
    raise NotFound(f"no compatible BUNDLE variant on release page (title={title!r}, rows={rows})")


def final_link(soup) -> str:
    btn = soup.select_one("a.downloadButton[href*='/download/']") or soup.select_one("a.downloadButton")
    if not btn:
        raise NotFound("download button missing")
    return urljoin(BASE, btn["href"])


def file_link(soup) -> str:
    a = soup.select_one("a[href*='download.php']") or soup.select_one("a[rel='nofollow'][href*='key=']")
    if not a:
        raise Blocked("final download link missing (blocked?)")
    return urljoin(BASE, a["href"])


def _run(c: Client, slug, version, device_sdk, dest_dir: Path) -> Path:
    c.html(BASE + "/")  # warm-up: cookies before deep pages
    _sleep()
    rel, rsoup = find_release(c, slug, version)
    log.info("release page %s", rel)
    _sleep()
    variant = pick_variant(rsoup, device_sdk)
    _sleep()
    vsoup = c.html(variant, referer=rel)
    dl_page = final_link(vsoup)
    _sleep()
    dsoup = c.html(dl_page, referer=variant)
    url = file_link(dsoup)
    _sleep(2, 4)
    dest = dest_dir / f"{slug.split('/')[-1]}-{version_slug(version)}.apkm"
    c.file(url, dl_page, dest)
    return dest


def download(slug: str, version: str, device_sdk: int, dest_dir: Path,
             flare_url: str = "", profile_hint: str = "") -> tuple:
    """(path, detail). Direct mode tries each browser profile (last good first) with backoff;
    with flare_url set, uses FlareSolverr once instead. Raises Blocked / NotFound."""
    if flare_url:
        return _run(Client("chrome", flare_url), slug, version, device_sdk, dest_dir), "flaresolverr"
    order = sorted(PROFILES, key=lambda p: p != profile_hint)
    last = None
    for n, prof in enumerate(order):
        try:
            client = Client(prof)
        except Exception as e:  # noqa: BLE001  (profile unsupported by the installed curl_cffi)
            log.warning("skipping profile %s: %s", prof, e)
            continue
        try:
            return _run(client, slug, version, device_sdk, dest_dir), prof
        except Blocked as e:
            last = e
            wait = BACKOFF_S[min(n, len(BACKOFF_S) - 1)] * random.uniform(0.8, 1.3)
            log.warning("blocked with profile %s (%s)%s", prof, e,
                        f"; waiting {wait:.0f}s" if n < len(order) - 1 else "")
            if n < len(order) - 1:
                time.sleep(wait)
    raise last or Blocked("no usable browser profile")
