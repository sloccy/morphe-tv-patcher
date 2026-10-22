from pathlib import Path

import pytest
from bs4 import BeautifulSoup

from patcher import apkmirror as A
from patcher import flaresolverr
from patcher.errors import Blocked, NotFound

FX = Path(__file__).parent / "fixtures"


def soup(name):
    return BeautifulSoup((FX / f"apkmirror-{name}.html").read_text(), "html.parser")


def test_pick_variant_from_real_release_page():
    url = A.pick_variant(soup("release"), 28)
    assert url.endswith("-armv7a-android-apk-download/")


def test_download_chain_links_from_real_pages():
    dl_page = A.final_link(soup("variant"))
    assert "/download/?key=" in dl_page
    assert "download.php?id=" in A.file_link(soup("download"))


def test_file_link_missing_means_blocked():
    with pytest.raises(Blocked):
        A.file_link(BeautifulSoup("<html>nothing</html>", "html.parser"))


class FakeClient:
    """Serves saved pages; the first (guessed) release URL 404s to force the app-page lookup."""
    def __init__(self):
        self.seen = []

    def html(self, url, referer=None):
        self.seen.append(url)
        if len(self.seen) == 1:
            raise NotFound(url)
        return soup("app")


def test_release_lookup_falls_back_to_app_page(monkeypatch):
    monkeypatch.setattr(A, "_sleep", lambda *a: None)
    slug = "amazon-mobile-llc/prime-video-android-tv-android-tv"
    found, page = A.find_release(FakeClient(), slug, "6.24.8+v16.0.0.361-allabis")
    assert found.endswith("6-24-8v16-0-0-361-allabis-release/") and page is not None


def test_not_found_page_served_as_200_is_detected():
    html = "<html><head><title>Page Not Found - APKMirror</title></head></html>"

    class C(A.Client):
        def __init__(self): self.flare_url, self.cookies, self.ua, self.sess = "x", {}, None, None
    import patcher.apkmirror as mod
    orig = mod.flaresolverr.solve
    mod.flaresolverr.solve = lambda *a, **k: {"html": html, "status": 200, "cookies": {}, "user_agent": ""}
    try:
        with pytest.raises(NotFound):
            C().html("https://www.apkmirror.com/x")
    finally:
        mod.flaresolverr.solve = orig


def test_release_lookup_not_found(monkeypatch):
    monkeypatch.setattr(A, "_sleep", lambda *a: None)
    with pytest.raises(NotFound):
        A.find_release(FakeClient(), "amazon-mobile-llc/prime-video-android-tv-android-tv", "0.0.1")


def test_flaresolverr_ok_and_error(monkeypatch):
    class R:
        def __init__(self, j): self.j = j
        def json(self): return self.j
    monkeypatch.setattr(flaresolverr.requests, "post", lambda *a, **k: R(
        {"status": "ok", "solution": {"response": "<html/>", "status": 200, "userAgent": "UA",
                                       "cookies": [{"name": "cf", "value": "1"}]}}))
    r = flaresolverr.solve("http://x", "https://y")
    assert r["cookies"] == {"cf": "1"} and r["user_agent"] == "UA"
    monkeypatch.setattr(flaresolverr.requests, "post", lambda *a, **k: R({"status": "error", "message": "boom"}))
    with pytest.raises(Blocked):
        flaresolverr.solve("http://x", "https://y")
